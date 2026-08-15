package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errFunctionDenied = errors.New("ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)")

// measuredDeadlock: byte-for-byte from a postgres:18 container, 2026-08-15.
const measuredDeadlock = "2026-08-15 10:00:34.543 UTC [25666] ERROR:  deadlock detected\n" +
	"2026-08-15 10:00:34.543 UTC [25666] DETAIL:  Process 25666 waits for ShareLock on transaction 948; blocked by process 25651.\n" +
	"\tProcess 25651 waits for ShareLock on transaction 949; blocked by process 25666.\n" +
	"\tProcess 25666: BEGIN; UPDATE yc_dl SET v=2 WHERE id=2; SELECT pg_sleep(2); UPDATE yc_dl SET v=2 WHERE id=1; COMMIT;\n" +
	"\tProcess 25651: BEGIN; UPDATE yc_dl SET v=1 WHERE id=1; SELECT pg_sleep(2); UPDATE yc_dl SET v=1 WHERE id=2; COMMIT;\n" +
	"2026-08-15 10:00:34.543 UTC [25666] HINT:  See server log for query details.\n" +
	"2026-08-15 10:00:34.543 UTC [25666] CONTEXT:  while updating tuple (0,1) in relation \"yc_dl\"\n" +
	"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  BEGIN; UPDATE yc_dl SET v=2 WHERE id=2; SELECT pg_sleep(2); UPDATE yc_dl SET v=2 WHERE id=1; COMMIT;\n"

const unrelatedTraffic = "2026-08-15 10:00:24.101 UTC [25640] LOG:  checkpoint starting: time\n" +
	"2026-08-15 10:00:24.980 UTC [25640] LOG:  checkpoint complete: wrote 12 buffers (0.1%)\n"

func nullable(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

type fakeLogQuerier struct {
	settings    logSettings
	settingsErr error

	logfiles    map[string]string
	functionErr error

	settingsReads int
	sql           []string
	args          []any
}

func (f *fakeLogQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args...)

	switch sql {
	case logSettingsSQL:
		f.settingsReads++

		if f.settingsErr != nil {
			return fakeRow{err: f.settingsErr}
		}

		s := f.settings

		return fakeRow{values: []any{
			nullable(s.dataDirectory), nullable(s.logDirectory), nullable(s.logFilename),
			nullable(s.loggingCollector), nullable(s.logDestination), nullable(s.serverAddr),
		}}

	case logLocationSQL:
		if f.functionErr != nil {
			return fakeRow{err: f.functionErr}
		}

		return fakeRow{values: []any{nullable(f.logfiles[""])}}

	case logLocationFormatSQL:
		if f.functionErr != nil {
			return fakeRow{err: f.functionErr}
		}

		format, _ := args[0].(string)

		return fakeRow{values: []any{nullable(f.logfiles[format])}}
	}

	return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

func (f *fakeLogQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, errors.New("the log tail reads no row sets")
}

func (f *fakeLogQuerier) Close(ctx context.Context) error { return nil }

func deniedQuerier(settings logSettings) *fakeLogQuerier {
	return &fakeLogQuerier{settings: settings, functionErr: errFunctionDenied}
}

type tailHarness struct {
	t *testing.T

	collector interface {
		Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
		WriteEpilogue(w io.Writer, s SampleContext) error
	}

	q *fakeLogQuerier

	index int
}

func (h *tailHarness) sampleContext() SampleContext {
	return SampleContext{
		At:       testWindowStart.Add(time.Duration(h.index) * DefaultLogTailInterval),
		Index:    h.index,
		Total:    12,
		Database: "orders_db",
		DBID:     "16401",
		redact:   func(err error) string { return errorText(err, "") },
	}
}

func (h *tailHarness) next() textBlock {
	h.t.Helper()

	h.index++

	var buf bytes.Buffer
	require.NoError(h.t, h.collector.Sample(context.Background(), h.q, &buf, h.sampleContext()))

	blocks := parseTextArtifact(h.t, buf.String())
	require.Len(h.t, blocks, 1, "one sample writes exactly one block")

	return blocks[0]
}

func (h *tailHarness) drain() []textBlock {
	h.t.Helper()

	var buf bytes.Buffer
	require.NoError(h.t, h.collector.WriteEpilogue(&buf, h.sampleContext()))

	return parseTextArtifact(h.t, buf.String())
}

func newDeadlockHarness(t *testing.T, q *fakeLogQuerier) *tailHarness {
	return &tailHarness{t: t, collector: NewDeadlocks(), q: q}
}

type textBlock struct {
	header string
	fields map[string]string
	body   string
}

func (b textBlock) has(key string) bool {
	_, ok := b.fields[key]

	return ok
}

func parseTextArtifact(t *testing.T, artifact string) []textBlock {
	t.Helper()

	var blocks []textBlock

	for data := artifact; data != ""; {
		require.True(t, strings.HasPrefix(data, "# engine=postgres "),
			"a block begins with its header, and the one before it ended exactly where bytes= said: %.80q", data)

		end := strings.IndexByte(data, '\n')
		require.GreaterOrEqual(t, end, 0, "an unterminated block header")

		block := textBlock{header: data[:end], fields: headerFields(t, data[:end])}
		data = data[end+1:]

		if raw, ok := block.fields["bytes"]; ok {
			size, err := strconv.Atoi(raw)
			require.NoError(t, err)
			require.LessOrEqual(t, size, len(data), "bytes= promises more than the file holds")

			block.body = data[:size]
			data = data[size:]
		}

		blocks = append(blocks, block)
	}

	return blocks
}

func headerFields(t *testing.T, header string) map[string]string {
	t.Helper()

	fields := map[string]string{}

	for _, token := range headerTokens(strings.TrimPrefix(header, "# ")) {
		key, value, found := strings.Cut(token, "=")
		if !found {
			continue
		}

		if strings.HasPrefix(value, `"`) {
			unquoted, err := strconv.Unquote(value)
			require.NoError(t, err, "a quoted header value must round-trip through strconv.Unquote")
			value = unquoted
		}

		fields[key] = value
	}

	return fields
}

func headerTokens(header string) []string {
	var (
		tokens  []string
		current strings.Builder
		quoted  bool
		escaped bool
	)

	for _, r := range header {
		switch {
		case escaped:
			current.WriteRune(r)
			escaped = false

		case quoted && r == '\\':
			current.WriteRune(r)
			escaped = true

		case r == '"':
			current.WriteRune(r)
			quoted = !quoted

		case r == ' ' && !quoted:
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}

		default:
			current.WriteRune(r)
		}
	}

	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}

	return tokens
}

type logDir struct {
	t *testing.T

	dataDirectory string
	logDirectory  string
	path          string
	format        logFormat
}

func newLogDir(t *testing.T) *logDir {
	t.Helper()

	dataDirectory := t.TempDir()
	logDirectory := filepath.Join(dataDirectory, "log")
	require.NoError(t, os.Mkdir(logDirectory, 0o755))

	d := &logDir{
		t:             t,
		dataDirectory: dataDirectory,
		logDirectory:  logDirectory,
		path:          filepath.Join(logDirectory, "postgresql-2026-08-15_100224.log"),
		format:        logFormatStderr,
	}

	require.NoError(t, os.WriteFile(d.path, nil, 0o644))

	return d
}

func (d *logDir) settings() logSettings {
	return logSettings{
		dataDirectory:    d.dataDirectory,
		logDirectory:     "log",
		logFilename:      "postgresql-%Y-%m-%d_%H%M%S.log",
		loggingCollector: "on",
		logDestination:   string(d.format),
		read:             true,
	}
}

func (d *logDir) writeCurrentLogfiles(lines ...string) {
	d.t.Helper()

	content := strings.Join(lines, "\n") + "\n"
	require.NoError(d.t, os.WriteFile(filepath.Join(d.dataDirectory, "current_logfiles"), []byte(content), 0o644))
}

func (d *logDir) append(content string) {
	d.t.Helper()

	f, err := os.OpenFile(d.path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(d.t, err)

	_, err = f.WriteString(content)
	require.NoError(d.t, err)
	require.NoError(d.t, f.Close())
}

func (d *logDir) rotate(name string) string {
	d.t.Helper()

	old := time.Now().Add(-time.Minute)
	require.NoError(d.t, os.Chtimes(d.path, old, old))

	d.path = filepath.Join(d.logDirectory, name)
	require.NoError(d.t, os.WriteFile(d.path, nil, 0o644))

	return d.path
}

const priorTraffic = "2026-08-15 09:59:58.000 UTC [1] LOG:  database system is ready to accept connections\n" +
	"2026-08-15 10:00:01.221 UTC [25610] LOG:  checkpoint starting: time\n"

type fakeLogConn struct {
	*fakeWindowConn

	q *fakeLogQuerier
}

func (c *fakeLogConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == currentDatabaseSQL {
		return c.fakeWindowConn.QueryRow(ctx, sql, args...)
	}

	return c.q.QueryRow(ctx, sql, args...)
}

func logGoldenClock(t *testing.T, samples int) *scriptedClock {
	t.Helper()

	preamble := at(32, 4, 980)
	start := at(32, 5, 0)

	times := []time.Time{preamble, preamble.Add(time.Millisecond), start}

	for i := range samples {
		tick := start.Add(time.Duration(i) * DefaultLogTailInterval)

		times = append(times,
			tick,
			tick.Add(20*time.Millisecond),
			tick.Add(21*time.Millisecond),
			tick.Add(22*time.Millisecond),
		)
	}

	closing := start.Add(time.Duration(samples) * DefaultLogTailInterval)

	return newScriptedClock(t, append(times, closing.Add(40*time.Millisecond), closing.Add(52*time.Millisecond))...)
}

func runLogGoldenWindow(t *testing.T, collector Collector, format logFormat,
	seed string, writes []string, duration time.Duration, clock *scriptedClock,
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	require.NoError(t, os.Mkdir("log", 0o755))

	name := "postgresql-2026-08-15_100224" + formatExtension(format)
	path := filepath.Join("log", name)

	require.NoError(t, os.WriteFile(path, []byte(seed), 0o644))
	require.NoError(t, os.WriteFile("current_logfiles",
		[]byte(string(format)+" log/"+name+"\n"), 0o644))

	settings := logSettings{
		dataDirectory:    ".",
		logDirectory:     "log",
		logFilename:      "postgresql-%Y-%m-%d_%H%M%S.log",
		loggingCollector: "on",
		logDestination:   string(format),
		read:             true,
	}

	writer := newFakeCollector("pg_log_writer")
	writer.artifact.Schedule = Every(DefaultLogTailInterval)

	tick := 0
	writer.sample = func(_ context.Context, s SampleContext, w io.Writer) error {
		if tick < len(writes) {
			appendFile(t, path, writes[tick])
		}

		tick++

		return writeBlockHeader(w, "pg_log_writer", "cluster",
			[]headerField{{"sample", strconv.Itoa(s.Index)}}, s.At)
	}

	window := &Window{
		Target:     testTarget(),
		Duration:   duration,
		Collectors: []Collector{collector, writer},
		now:        clock.now,
		after:      clock.after,
		connect: connectTo(&fakeLogConn{
			fakeWindowConn: newFakeWindowConn(),
			q:              deniedQuerier(settings),
		}),
	}

	return window.Run(context.Background())
}

func runRemoteGoldenWindow(t *testing.T, collector Collector, duration time.Duration,
	clock *scriptedClock,
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	settings := logSettings{
		logDirectory:     "/var/lib/postgresql/18/docker/log",
		logFilename:      "postgresql-%Y-%m-%d_%H%M%S.log",
		loggingCollector: "on",
		logDestination:   "stderr",

		serverAddr: "203.0.113.7",
		read:       true,
	}

	q := &fakeLogQuerier{
		settings: settings,
		logfiles: map[string]string{
			"stderr": "/var/lib/postgresql/18/docker/log/postgresql-2026-08-15_000000.log",
		},
	}

	writer := newFakeCollector("pg_log_writer")
	writer.artifact.Schedule = Every(DefaultLogTailInterval)

	window := &Window{
		Target:     testTarget(),
		Duration:   duration,
		Collectors: []Collector{collector, writer},
		now:        clock.now,
		after:      clock.after,
		connect: connectTo(&fakeLogConn{
			fakeWindowConn: newFakeWindowConn(),
			q:              q,
		}),
	}

	return window.Run(context.Background())
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)

	_, err = f.WriteString(content)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestResolveLogSourceRoutes(t *testing.T) {
	t.Run("current_logfiles is read from disk before the function is asked", func(t *testing.T) {
		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

		q := deniedQuerier(dir.settings())

		source := resolveLogSource(context.Background(), q, dir.settings(), nil)

		assert.Equal(t, resolvedByCurrentLogfiles, source.resolvedBy)
		assert.Equal(t, dir.path, source.path)
		assert.Equal(t, logFormatStderr, source.format)
		assert.Empty(t, source.reason)
		assert.Empty(t, q.sql, "the disk answered, so the connection was never asked")
	})

	t.Run("a denied function falls through to the disk - the 14 to 16 case", func(t *testing.T) {
		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

		source := resolveLogSource(context.Background(), deniedQuerier(dir.settings()), dir.settings(), nil)

		require.Equal(t, ModeDBHost, source.captureMode(),
			"pg_monitor cannot execute pg_current_logfile() before PostgreSQL 17, and before this "+
				"the mode went down with that statement on three of the five supported versions")
		assert.Equal(t, resolvedByCurrentLogfiles, source.resolvedBy)
	})

	t.Run("the function carries an unreadable data directory", func(t *testing.T) {
		dir := newLogDir(t)

		settings := dir.settings()
		settings.dataDirectory = filepath.Join(dir.dataDirectory, "not-readable")
		settings.logDirectory = dir.logDirectory

		q := &fakeLogQuerier{settings: settings, logfiles: map[string]string{"stderr": dir.path}}

		source := resolveLogSource(context.Background(), q, settings, nil)

		assert.Equal(t, resolvedByFunction, source.resolvedBy)
		assert.Equal(t, dir.path, source.path)
		assert.Empty(t, source.reason)
	})

	t.Run("the glob carries a denied function with no readable data directory", func(t *testing.T) {
		dir := newLogDir(t)

		settings := dir.settings()
		settings.dataDirectory = ""
		settings.logDirectory = dir.logDirectory

		newest := dir.rotate("postgresql-2026-08-15_110000.log")

		source := resolveLogSource(context.Background(), deniedQuerier(settings), settings, nil)

		assert.Equal(t, resolvedByGlob, source.resolvedBy)
		assert.Equal(t, newest, source.path, "the newest match, and it is labelled a guess")
		assert.Empty(t, source.reason)
	})

	t.Run("the privilege floor has no route at all", func(t *testing.T) {
		settings := logSettings{loggingCollector: "on", logDestination: "stderr", read: true}

		source := resolveLogSource(context.Background(), deniedQuerier(settings), settings, nil)

		assert.Equal(t, reasonUnresolved, source.reason)
		assert.Equal(t, ModeRemote, source.captureMode())
		assert.Empty(t, source.path)
	})

	t.Run("logging_collector off is a cause of its own", func(t *testing.T) {
		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

		settings := dir.settings()
		settings.loggingCollector = "off"

		q := &fakeLogQuerier{settings: settings}

		source := resolveLogSource(context.Background(), q, settings, nil)

		assert.Equal(t, reasonCollectorOff, source.reason)
		assert.Empty(t, q.sql, "there is no file for a route to find, so none is tried")
	})

	t.Run("an unreadable file resolves and says so", func(t *testing.T) {
		requireUnprivileged(t)

		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
		require.NoError(t, os.Chmod(dir.path, 0o000))

		source := resolveLogSource(context.Background(), deniedQuerier(dir.settings()), dir.settings(), nil)

		assert.Equal(t, reasonUnreadable, source.reason,
			"the default outcome of a correct-looking Mode H deployment: log files are 0600")
		assert.Equal(t, dir.path, source.path, "and the path is named, so an operator knows what to chmod")
		assert.Equal(t, ModeRemote, source.captureMode())
	})

	t.Run("a failed settings read is a different cause from an empty answer", func(t *testing.T) {
		source := resolveLogSource(context.Background(), deniedQuerier(logSettings{}), logSettings{}, nil)

		assert.Equal(t, reasonModeUnknown, source.reason)
		assert.Equal(t, ModeUnknown, source.captureMode(),
			"detection could not run, which is a different sentence from detection finding nothing")
	})
}

func TestCurrentLogfilesParsing(t *testing.T) {
	for _, tt := range []struct {
		name       string
		content    string
		wantFormat logFormat
		wantSuffix string
		wantAll    []logFormat
		wantOK     bool
	}{
		{
			name:       "one destination",
			content:    "stderr log/postgresql-2026-08-15_100224.log\n",
			wantFormat: logFormatStderr,
			wantSuffix: "log/postgresql-2026-08-15_100224.log",
			wantAll:    []logFormat{logFormatStderr},
			wantOK:     true,
		},
		{
			name: "three destinations, most structured first",
			content: "stderr log/postgresql-2026-08-15_100224.log\n" +
				"csvlog log/postgresql-2026-08-15_100224.csv\n" +
				"jsonlog log/postgresql-2026-08-15_100224.json\n",
			wantFormat: logFormatJSON,
			wantSuffix: "log/postgresql-2026-08-15_100224.json",
			wantAll:    []logFormat{logFormatJSON, logFormatCSV, logFormatStderr},
			wantOK:     true,
		},
		{
			name:       "no trailing newline",
			content:    "csvlog log/postgresql-2026-08-15_100224.csv",
			wantFormat: logFormatCSV,
			wantSuffix: "log/postgresql-2026-08-15_100224.csv",
			wantAll:    []logFormat{logFormatCSV},
			wantOK:     true,
		},
		{
			name:       "an absolute path is taken as it stands",
			content:    "stderr /var/log/postgresql/postgresql.log\n",
			wantFormat: logFormatStderr,
			wantSuffix: "/var/log/postgresql/postgresql.log",
			wantAll:    []logFormat{logFormatStderr},
			wantOK:     true,
		},
		{
			name:       "a malformed line is skipped rather than fatal",
			content:    "nonsense\nsyslog\nstderr log/postgresql-2026-08-15_100224.log\n",
			wantFormat: logFormatStderr,
			wantSuffix: "log/postgresql-2026-08-15_100224.log",
			wantAll:    []logFormat{logFormatStderr},
			wantOK:     true,
		},
		{
			name:    "nothing this agent can read",
			content: "syslog something\n",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataDirectory := t.TempDir()
			require.NoError(t, os.WriteFile(
				filepath.Join(dataDirectory, "current_logfiles"), []byte(tt.content), 0o644))

			raw, format, available, ok := resolveFromCurrentLogfiles(logSettings{dataDirectory: dataDirectory})

			require.Equal(t, tt.wantOK, ok)
			if !tt.wantOK {
				return
			}

			assert.Equal(t, tt.wantFormat, format)
			assert.Equal(t, tt.wantSuffix, raw)
			assert.Equal(t, tt.wantAll, available)
		})
	}
}

func TestGlobAppliesTheExtensionSubstitution(t *testing.T) {
	dir := newLogDir(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir.logDirectory, "postgresql-2026-08-15_100224.csv"), []byte("x\n"), 0o644))

	settings := dir.settings()
	settings.logDestination = "csvlog"

	raw, format, _, ok := resolveFromGlob(settings)

	require.True(t, ok)
	assert.Equal(t, logFormatCSV, format)
	assert.True(t, strings.HasSuffix(raw, ".csv"), "resolved %s", raw)
}

func TestGlobPatternReplacesEveryStrftimeEscape(t *testing.T) {
	assert.Equal(t, "postgresql-*-*-*_*.log", globPattern("postgresql-%Y-%m-%d_%H%M%S.log"),
		"the literal separators are kept and only the escapes widen, so the pattern stays "+
			"as narrow as guessing the rotation allows")
	assert.Equal(t, "postgresql-*.log", globPattern("postgresql-%a.log"))
	assert.Equal(t, "postgresql.log", globPattern("postgresql.log"))

	assert.Equal(t, `pg\[1\].log`, globPattern("pg[1].log"),
		"a glob metacharacter in a customer's log_filename must not widen the pattern silently")
}

func TestSubstituteExtensionFollowsThePostgresRule(t *testing.T) {
	assert.Equal(t, "postgresql-%a.csv", substituteExtension("postgresql-%a.log", logFormatCSV))
	assert.Equal(t, "postgresql-%a.json", substituteExtension("postgresql-%a.log", logFormatJSON))
	assert.Equal(t, "postgresql-%a.log", substituteExtension("postgresql-%a.log", logFormatStderr))

	assert.Equal(t, "pglog.csv", substituteExtension("pglog", logFormatCSV),
		"where there is no .log to replace, the extension is appended")
}

func TestTailAnnouncesTheSourceOnceAndPrefersTheStructuredFormat(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles(
		"stderr log/postgresql-2026-08-15_100224.log",
		"csvlog log/postgresql-2026-08-15_100224.csv",
		"jsonlog log/postgresql-2026-08-15_100224.json",
	)
	require.NoError(t, os.WriteFile(
		filepath.Join(dir.logDirectory, "postgresql-2026-08-15_100224.csv"), nil, 0o644))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir.logDirectory, "postgresql-2026-08-15_100224.json"), nil, 0o644))

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))

	first := h.next()
	assert.Equal(t, "jsonlog", first.fields["log_format"])
	assert.Equal(t, "jsonlog,csvlog,stderr", first.fields["log_formats"],
		"a bundle says what it could have read as well as what it did")
	assert.Equal(t, matchedBySQLState, first.fields["matched_by"])
	assert.Equal(t, ModeDBHost, first.fields["capture_mode"])
	assert.NotEmpty(t, first.fields["log_path"])

	second := h.next()
	assert.False(t, second.has("log_formats"), "the source's identity is announced once")
	assert.False(t, second.has("log_path"))
	assert.False(t, second.has("capture_mode"))
	assert.Equal(t, "jsonlog", second.fields["log_format"], "and the format rides every block")
}

func TestTailSampleOneSeeksToEOF(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
	dir.append(measuredDeadlock)

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))

	first := h.next()

	assert.Equal(t, "0", first.fields["matched"],
		"the window is the evidence: a deadlock logged before t0 belongs to a capture that did not happen")
	assert.Equal(t, first.fields["from_offset"], first.fields["to_offset"])
	assert.Equal(t, strconv.Itoa(len(measuredDeadlock)), first.fields["from_offset"])
	assert.Empty(t, first.body)

	assert.False(t, first.has("resolved_late"),
		"a tail that resolved on sample 1 seeks past nothing the window could have covered, "+
			"so it has no gap to declare")
}

func TestTailLateResolutionDeclaresTheIntervalItNeverRead(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	q := deniedQuerier(dir.settings())
	q.settingsErr = errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	h := newDeadlockHarness(t, q)

	require.Equal(t, reasonModeUnknown, h.next().fields["reason"])

	dir.append(measuredDeadlock + unrelatedTraffic)
	q.settingsErr = nil

	second := h.next()

	assert.Equal(t, "true", second.fields["resolved_late"],
		"coverage begins at this from_offset rather than at t0, and the block says so")
	assert.Equal(t, "0", second.fields["matched"])
	assert.Equal(t, second.fields["from_offset"], second.fields["to_offset"])

	dir.append(unrelatedTraffic)

	assert.False(t, h.next().has("resolved_late"),
		"declared once: it is where the file's coverage starts, not something a read did")
}

func TestTailReopensARotationTargetItCouldNotOpenAtTheTime(t *testing.T) {
	requireUnprivileged(t)

	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
	require.Equal(t, resolvedByCurrentLogfiles, h.next().fields["log_resolved_by"])

	dir.rotate("postgresql-2026-08-15_110000.log")
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_110000.log")
	require.NoError(t, os.Chmod(dir.path, 0o000))

	notAdopted := h.next()
	assert.False(t, notAdopted.has("rotated"), "the tail still holds the file it was reading")

	assert.Equal(t, reasonUnreadable, h.next().fields["reason"],
		"and the sample after it names the target it could not open")

	require.NoError(t, os.Chmod(dir.path, 0o644))
	dir.append(measuredDeadlock + unrelatedTraffic)

	recovered := h.next()

	assert.Equal(t, "0", recovered.fields["from_offset"],
		"a file that appeared inside the window is read whole")
	assert.Equal(t, "1", recovered.fields["matched"])
	assert.Equal(t, measuredDeadlock, recovered.body)

	assert.False(t, recovered.has("resolved_late"), "nothing was skipped, so there is nothing to declare")
}

func TestTailResumesTheFileItLostRatherThanRereadingIt(t *testing.T) {
	requireUnprivileged(t)

	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
	h.next()

	original := dir.path
	dir.append(measuredDeadlock + unrelatedTraffic)
	require.Equal(t, "1", h.next().fields["matched"])

	rotated := dir.rotate("postgresql-2026-08-15_110000.log")
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_110000.log")
	require.NoError(t, os.Chmod(rotated, 0o000))
	h.next()

	dir.path = original
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
	dir.append(measuredDeadlock + unrelatedTraffic)

	resumed := h.next()

	assert.Equal(t, "1", resumed.fields["matched"], "the event it had not read, and not the one it had")
	assert.Equal(t, measuredDeadlock, resumed.body)
}

func TestTailOffsetsAdvanceWithoutGapOrOverlap(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))

	blocks := []textBlock{h.next()}

	for range 3 {
		dir.append(unrelatedTraffic)
		blocks = append(blocks, h.next())
	}

	for i, block := range blocks {
		if i == 0 {
			continue
		}

		assert.Equal(t, blocks[i-1].fields["to_offset"], block.fields["from_offset"],
			"every byte belongs to exactly one block: a gap loses evidence and an overlap invents it")
	}

	assert.Equal(t, strconv.Itoa(3*len(unrelatedTraffic)), blocks[len(blocks)-1].fields["to_offset"])
}

func TestTailFollowsRotationThroughEveryRoute(t *testing.T) {
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, dir *logDir) (*fakeLogQuerier, logSettings)
		after func(dir *logDir, q *fakeLogQuerier)
		route string
	}{
		{
			name: "current_logfiles",
			setup: func(t *testing.T, dir *logDir) (*fakeLogQuerier, logSettings) {
				dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

				return deniedQuerier(dir.settings()), dir.settings()
			},
			after: func(dir *logDir, q *fakeLogQuerier) {
				dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_110000.log")
			},
			route: resolvedByCurrentLogfiles,
		},
		{
			name: "pg_current_logfile",
			setup: func(t *testing.T, dir *logDir) (*fakeLogQuerier, logSettings) {
				settings := dir.settings()
				settings.dataDirectory = filepath.Join(dir.dataDirectory, "unreadable")
				settings.logDirectory = dir.logDirectory

				return &fakeLogQuerier{
					settings: settings,
					logfiles: map[string]string{"stderr": dir.path},
				}, settings
			},
			after: func(dir *logDir, q *fakeLogQuerier) {
				q.logfiles["stderr"] = dir.path
			},
			route: resolvedByFunction,
		},
		{
			name: "glob",
			setup: func(t *testing.T, dir *logDir) (*fakeLogQuerier, logSettings) {
				settings := dir.settings()
				settings.dataDirectory = ""
				settings.logDirectory = dir.logDirectory

				return deniedQuerier(settings), settings
			},
			after: func(dir *logDir, q *fakeLogQuerier) {},
			route: resolvedByGlob,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := newLogDir(t)
			q, _ := tt.setup(t, dir)

			h := newDeadlockHarness(t, q)

			first := h.next()
			require.Equal(t, tt.route, first.fields["log_resolved_by"])

			generation := measuredDeadlock + unrelatedTraffic
			dir.append(generation)

			superseded := dir.path
			dir.rotate("postgresql-2026-08-15_110000.log")
			tt.after(dir, q)
			dir.append(generation)

			rotated := h.next()

			assert.Equal(t, "true", rotated.fields["rotated"])
			assert.Equal(t, superseded, rotated.fields["log_path_previous"])
			assert.Equal(t, dir.path, rotated.fields["log_path"])
			assert.Equal(t, "2", rotated.fields["matched"],
				"the old handle is drained to its own EOF before the new file is opened")
			assert.Equal(t, "0", rotated.fields["from_offset"], "and the new file is read from its start")
			assert.Equal(t, strconv.Itoa(len(generation)), rotated.fields["previous_to_offset"])
			assert.Equal(t, strconv.Itoa(len(generation)), rotated.fields["to_offset"])
			assert.Equal(t, measuredDeadlock+measuredDeadlock, rotated.body)
		})
	}
}

func TestTailGlobRouteDrainsEveryGenerationInOrder(t *testing.T) {
	dir := newLogDir(t)

	settings := dir.settings()
	settings.dataDirectory = ""
	settings.logDirectory = dir.logDirectory

	h := newDeadlockHarness(t, deniedQuerier(settings))

	require.Equal(t, resolvedByGlob, h.next().fields["log_resolved_by"])

	dir.append(measuredDeadlock + unrelatedTraffic)
	dir.rotate("postgresql-2026-08-15_110000.log")
	dir.append(measuredDeadlock + unrelatedTraffic)

	rotated := h.next()

	assert.Equal(t, "true", rotated.fields["rotated"])
	assert.Equal(t, measuredDeadlock+measuredDeadlock, rotated.body,
		"nothing duplicated and nothing skipped, in order")
}

func TestTailClosesTheSupersededHandleOnRotation(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	collector := NewDeadlocks()
	h := &tailHarness{t: t, collector: collector, q: deniedQuerier(dir.settings())}

	h.next()
	superseded := collector.tail.file

	dir.rotate("postgresql-2026-08-15_110000.log")
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_110000.log")

	h.next()

	require.NotSame(t, superseded, collector.tail.file, "a new handle")
	_, err := superseded.Stat()
	assert.Error(t, err, "and the old one is closed rather than held for the run")
}

func TestTailDetectsTruncationInPlace(t *testing.T) {
	t.Run("shorter than the saved offset", func(t *testing.T) {
		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

		seeded := strings.Repeat(unrelatedTraffic, 4) + measuredDeadlock
		dir.append(seeded)

		h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
		require.Equal(t, strconv.Itoa(len(seeded)), h.next().fields["to_offset"])

		rewritten := unrelatedTraffic + measuredDeadlock + unrelatedTraffic
		require.Less(t, len(rewritten), len(seeded), "shorter, so only the size check can catch it")
		require.True(t, strings.HasPrefix(rewritten, unrelatedTraffic), "and the head is unchanged")

		require.NoError(t, os.WriteFile(dir.path, []byte(rewritten), 0o644))

		block := h.next()

		assert.Equal(t, "true", block.fields["file_truncated"])
		assert.Equal(t, "1", block.fields["matched"], "and the tail reads the new content from 0")

		assert.Equal(t, "0", block.fields["from_offset"],
			"the offsets restart with the file, exactly as a rotation's do")
		assert.Equal(t, strconv.Itoa(len(seeded)), block.fields["previous_to_offset"],
			"and the superseded stream says where it ended, so from_offset never points into "+
				"bytes that no longer exist and to_offset minus from_offset is never negative")
		assert.Equal(t, strconv.Itoa(len(rewritten)), block.fields["to_offset"])
	})

	t.Run("truncated and regrown past the saved offset", func(t *testing.T) {
		dir := newLogDir(t)
		dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
		dir.append(unrelatedTraffic)

		h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
		h.next()

		regrown := "2026-08-15 11:00:00.000 UTC [1] LOG:  database system is ready\n" +
			measuredDeadlock + unrelatedTraffic
		require.Greater(t, len(regrown), len(unrelatedTraffic))
		require.NoError(t, os.WriteFile(dir.path, []byte(regrown), 0o644))

		block := h.next()

		assert.Equal(t, "true", block.fields["file_truncated"])
		assert.Equal(t, "1", block.fields["matched"])
		assert.Equal(t, measuredDeadlock, block.body)

		assert.Equal(t, "0", block.fields["from_offset"], "rebased, as the size check's case is")
		assert.Equal(t, strconv.Itoa(len(unrelatedTraffic)), block.fields["previous_to_offset"])
	})
}

func TestTailFailedResolutionRetriesOnTheNextSample(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	q := deniedQuerier(dir.settings())
	q.settingsErr = errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	h := newDeadlockHarness(t, q)

	first := h.next()
	assert.Equal(t, reasonModeUnknown, first.fields["reason"],
		"a settings read that timed out during exactly the spike the capture exists for "+
			"costs one block, not twelve")
	assert.Contains(t, first.fields["error"], "statement timeout")

	q.settingsErr = nil

	second := h.next()
	assert.False(t, second.has("reason"))
	assert.Equal(t, resolvedByCurrentLogfiles, second.fields["log_resolved_by"])
}

func TestTailEveryReasonWritesAHeaderOnlyBlockWithNoMatchedKey(t *testing.T) {
	dir := newLogDir(t)

	collectorOff := dir.settings()
	collectorOff.loggingCollector = "off"

	floor := logSettings{loggingCollector: "on", read: true}

	for _, tt := range []struct {
		name     string
		settings logSettings
		setup    func(t *testing.T)
		reason   string
		mode     string
	}{
		{name: "collector off", settings: collectorOff, reason: reasonCollectorOff, mode: ModeRemote},
		{name: "unresolved", settings: floor, reason: reasonUnresolved, mode: ModeRemote},
		{name: "mode unknown", settings: logSettings{}, reason: reasonModeUnknown, mode: ModeUnknown},
		{
			name:     "unreadable",
			settings: dir.settings(),
			setup: func(t *testing.T) {
				requireUnprivileged(t)
				dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
				require.NoError(t, os.Chmod(dir.path, 0o000))
			},
			reason: reasonUnreadable,
			mode:   ModeRemote,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t)
			}

			q := deniedQuerier(tt.settings)
			if tt.reason == reasonModeUnknown {
				q.settingsErr = errors.New("ERROR: canceling statement due to statement timeout")
			}

			block := newDeadlockHarness(t, q).next()

			assert.Equal(t, tt.reason, block.fields["reason"])
			assert.Equal(t, tt.mode, block.fields["capture_mode"])

			assert.False(t, block.has("matched"),
				"this is the only assertion in the package that a key is NOT there, and "+
					"direction §3's never-0 sentence is why")

			assert.Equal(t, "0", block.fields["bytes"], "and the block is header-only")
			assert.Empty(t, block.body)
		})
	}
}

func TestTailWritesABlockForEveryScheduledSample(t *testing.T) {
	h := newDeadlockHarness(t, deniedQuerier(logSettings{loggingCollector: "on", read: true}))

	for i := 1; i <= 12; i++ {
		block := h.next()

		assert.Equal(t, strconv.Itoa(i), block.fields["sample"])
		assert.Equal(t, reasonUnresolved, block.fields["reason"])
	}
}

func TestTailScanCapSkipsToThePresentAndSaysSo(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
	h.next()

	dir.append("2026-08-15 10:00:35.000 UTC [1] LOG:  incomplete")
	require.Equal(t, "0", h.next().fields["matched"])

	filler := strings.Repeat("2026-08-15 10:00:36.000 UTC [1] LOG:  noise\n", (MaxScanBytes/43)+64)
	dir.append(filler)
	dir.append(measuredDeadlock)

	block := h.next()

	assert.Equal(t, "true", block.fields["scan_truncated"])
	assert.NotEmpty(t, block.fields["skipped_bytes"])
	assert.Equal(t, "true", block.fields["carry_dropped"])
	assert.Equal(t, "0", block.fields["matched"],
		"skipped_bytes= is a different claim from matched=0, and the header keeps them different")
}

func TestTailDrainIsNotASampleAndSurvivesADeadWindow(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	h := newDeadlockHarness(t, deniedQuerier(dir.settings()))
	h.next()

	dir.append(measuredDeadlock)

	blocks := h.drain()
	require.Len(t, blocks, 1)

	assert.Equal(t, "true", blocks[0].fields["drain"])
	assert.False(t, blocks[0].has("sample"),
		"samples_written stays arithmetic about the schedule: the drain is a thirteenth block, "+
			"never a thirteenth sample")
	assert.Equal(t, "1", blocks[0].fields["matched"],
		"the final interval Every's offsets leave open is what this closes")
	assert.Equal(t, measuredDeadlock, blocks[0].body)

	assert.Equal(t, "true", blocks[0].fields["partial_event"],
		"an event ending exactly at EOF is complete but unprovably so - the bytes are intact "+
			"either way, and the flag means the tail could not prove the end")
}

func TestTailDrainNoOpsWhenTheTailNeverResolved(t *testing.T) {
	h := newDeadlockHarness(t, deniedQuerier(logSettings{}))

	assert.Empty(t, h.drain(),
		"closeArtifacts runs on the connect-failure path too, and an artifact with nothing "+
			"to drain keeps its preamble-plus-closing-block shape")
}

func TestTailDrainClosesTheHandle(t *testing.T) {
	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")

	collector := NewDeadlocks()
	h := &tailHarness{t: t, collector: collector, q: deniedQuerier(dir.settings())}

	h.next()
	require.NotNil(t, collector.tail.file)

	h.drain()
	assert.Nil(t, collector.tail.file, "the drain is the collector's last call, so it is its cleanup")
}

func TestLogLocalityIsRecordedRatherThanGated(t *testing.T) {
	assert.Equal(t, localityLocal, logLocality(logSettings{read: true}),
		"a unix-socket connection is proof the server is this host")

	assert.Equal(t, localityLocal, logLocality(logSettings{serverAddr: "127.0.0.1", read: true}))

	assert.Equal(t, localityRemote, logLocality(logSettings{serverAddr: "203.0.113.7", read: true}),
		"same-packaging fleets share a data_directory string, so an agent pointed at another "+
			"host would resolve its own log - recorded as a contradiction, never gated")

	assert.Equal(t, localityUnknown, logLocality(logSettings{}))
}
