package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testWindowStart = time.Date(2026, 8, 7, 14, 32, 4, 980_000_000, time.UTC)

func testWindowTarget() Target {
	target := testTarget()
	target.Database = "orders_configured"

	return target
}

type fakeClock struct {
	current time.Time

	waits []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{current: testWindowStart} }

func (c *fakeClock) now() time.Time { return c.current }

func (c *fakeClock) advance(d time.Duration) { c.current = c.current.Add(d) }

func (c *fakeClock) after(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)

	if d > 0 {
		c.advance(d)
	}

	fired := make(chan time.Time, 1)
	fired <- c.current

	return fired
}

type fakeCollector struct {
	artifact Artifact
	sample   func(ctx context.Context, s SampleContext, w io.Writer) error

	seen      []SampleContext
	deadlines []time.Time
}

func newFakeCollector(name string) *fakeCollector {
	return &fakeCollector{artifact: Artifact{
		Name:     name,
		FileName: name + ".txt",
		Scope:    "database",
		Schedule: StartEnd(),
	}}
}

func (f *fakeCollector) Artifact() Artifact { return f.artifact }

func (f *fakeCollector) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	f.seen = append(f.seen, s)

	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)

	if f.sample != nil {
		return f.sample(ctx, s, w)
	}

	if err := writeBlockHeader(w, "fake_view", f.artifact.Scope, []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}, s.At); err != nil {
		return err
	}

	return writeRows(w, []string{"relid"}, [][]string{{"16390"}})
}

// fakePrologueCollector is a fake that has something to write before the
// connection exists. It proves Prologue against the window alone: no collector
// in the package needs to be linked for the mechanism to be exercised.
type fakePrologueCollector struct {
	*fakeCollector

	prologue func(w io.Writer, s SampleContext) error

	seenPrologue []SampleContext
}

func newFakePrologueCollector(name string) *fakePrologueCollector {
	return &fakePrologueCollector{fakeCollector: newFakeCollector(name)}
}

func (f *fakePrologueCollector) WritePrologue(w io.Writer, s SampleContext) error {
	f.seenPrologue = append(f.seenPrologue, s)

	if f.prologue != nil {
		return f.prologue(w, s)
	}

	return writeBlockHeader(w, f.artifact.Name+"_target", f.artifact.Scope, []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
	}, s.At)
}

type fakeWindowConn struct {
	database    string
	dbid        *string
	identifyErr error

	closed bool
}

func newFakeWindowConn() *fakeWindowConn {
	return &fakeWindowConn{database: "orders_db", dbid: ptr("16401")}
}

func (c *fakeWindowConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql != currentDatabaseSQL {
		return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}

	if c.identifyErr != nil {
		return fakeRow{err: c.identifyErr}
	}

	return fakeRow{values: []any{c.database, c.dbid}}
}

func (c *fakeWindowConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, errors.New("the window reads no row sets; its collectors do")
}

func (c *fakeWindowConn) Close(ctx context.Context) error {
	c.closed = true
	return nil
}

func newTestWindow(t *testing.T, clock *fakeClock, collectors ...Collector) *Window {
	t.Helper()
	t.Chdir(t.TempDir())

	return &Window{
		Target:     testWindowTarget(),
		Duration:   120 * time.Second,
		Collectors: collectors,
		now:        clock.now,
		after:      clock.after,
		connect: func(ctx context.Context, target Target) (windowConn, error) {
			return newFakeWindowConn(), nil
		},
	}
}

func artifactText(t *testing.T, result ArtifactResult) string {
	t.Helper()

	content, err := os.ReadFile(result.Artifact.FileName)
	require.NoError(t, err)

	return string(content)
}

func headersOf(t *testing.T, result ArtifactResult) []string {
	t.Helper()

	var headers []string
	for line := range strings.SplitSeq(strings.TrimSuffix(artifactText(t, result), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			headers = append(headers, line)
		}
	}

	return headers
}

func TestWindowStartEndWritesTwoSamples(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Len(t, results, 1)
	assert.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 2, results[0].SamplesExpected)
	assert.Equal(t, 2, results[0].SamplesWritten)
	assert.NoError(t, results[0].IOErr)

	require.Len(t, collector.seen, 2)
	assert.Equal(t, 1, collector.seen[0].Index)
	assert.Equal(t, 2, collector.seen[1].Index)

	assert.Equal(t, testWindowStart, collector.seen[0].At, "sample 1 is at t0")
	assert.Equal(t, testWindowStart.Add(120*time.Second), collector.seen[1].At,
		"sample 2 is at t0+duration")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 4, "preamble, two samples, closing block")
	assert.Contains(t, headers[0],
		"status=started window=120s schedule=start_end interval= samples_expected=2")
	assert.Contains(t, headers[1], "sample=1")
	assert.Contains(t, headers[2], "sample=2")
	assert.Contains(t, headers[3], "status=complete samples_expected=2 samples_written=2")
}

func TestWindowSecondSampleTargetsTheWindowEndNotTheSampleEnd(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(30 * time.Second)
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Equal(t, []time.Duration{90 * time.Second}, clock.waits,
		"the wait is duration minus elapsed, not the whole duration")

	assert.Equal(t, testWindowStart.Add(120*time.Second), collector.seen[1].At,
		"sample 2 still lands at t0+duration")
	assert.Equal(t, StatusComplete, results[0].Status)
}

func TestWindowDoesNotWaitWhenTheFirstSampleOverranTheWindow(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(200 * time.Second)
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.Empty(t, clock.waits, "an overdue tick is not waited on at all")
	assert.Equal(t, 2, results[0].SamplesWritten, "sample 2 still happens, immediately")
	assert.Equal(t, testWindowStart.Add(200*time.Second), collector.seen[1].At,
		"and carries the ts= at which it actually happened")
}

func TestWindowWritesThePreambleBeforeConnecting(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var atConnect string
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		content, err := os.ReadFile(collector.artifact.FileName)
		require.NoError(t, err, "the artifact must exist before the connection is attempted")
		atConnect = string(content)

		return newFakeWindowConn(), nil
	}

	window.Run(context.Background())

	assert.Contains(t, atConnect, "status=started",
		"the preamble must be on disk, and synced, before Connect")
	assert.Contains(t, atConnect, "samples_expected=2",
		"a file that stops here still says how many samples were meant to be in it")
}

func TestWindowPreambleCarriesTheConfiguredDatabaseAndNoOID(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	results := newTestWindow(t, clock, collector).Run(context.Background())

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[0], "db=orders_configured dbid= status=started")
	assert.Contains(t, headers[1], "db=orders_db dbid=16401",
		"every block after the connection carries current_database() and its OID")
	assert.Contains(t, headers[3], "db=orders_db dbid=16401",
		"including the closing block")
}

func TestWindowPreambleDeclaresTheSchedule(t *testing.T) {
	clock := newFakeClock()

	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(10 * time.Second)

	edges := newFakeCollector("pg_edges")

	results := newTestWindow(t, clock, interval, edges).Run(context.Background())

	assert.Contains(t, headersOf(t, results[0])[0],
		"window=120s schedule=every interval=10s samples_expected=12",
		"twelve samples is only a fact if the reader is told the cadence")

	assert.Contains(t, headersOf(t, results[1])[0],
		"window=120s schedule=start_end interval= samples_expected=2",
		"both keys for both schedules, empty where the cadence does not apply")
}

func TestWindowConnectFailureDoesNotCostTheWindow(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		return nil, ErrTooManyConnections
	}

	results := window.Run(context.Background())

	assert.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)
	assert.Empty(t, collector.seen, "a refused connection takes no samples")
	assert.Empty(t, clock.waits, "and does not wait out the window")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 2, "a connect failure is two lines: what was attempted, and why not")
	assert.Contains(t, headers[1],
		"db=orders_configured dbid= status=connect_failed "+
			"samples_expected=2 samples_written=0 connect_error=too_many_connections")

	assert.NotEmpty(t, artifactText(t, results[0]),
		"the file is never zero bytes, so the upload path cannot drop it")
}

func TestWindowCancelledBetweenSamples(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(sampleCtx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			cancel()
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(ctx)

	assert.Equal(t, StatusCancelled, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Len(t, collector.seen, 1)

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[len(headers)-1],
		"status=cancelled samples_expected=2 samples_written=1")
}

func TestWindowCancelledBetweenIntervalTicks(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	collector := newFakeCollector("pg_interval")
	collector.artifact.Schedule = Every(10 * time.Second)
	collector.sample = func(sampleCtx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 3 {
			cancel()
			clock.advance(45 * time.Second)
		}

		return nil
	}

	results := newTestWindow(t, clock, collector).Run(ctx)

	assert.Equal(t, StatusCancelled, results[0].Status)
	assert.Equal(t, 12, results[0].SamplesExpected)
	assert.Equal(t, 3, results[0].SamplesWritten)
	assert.Len(t, collector.seen, 3, "no tick runs after the cancellation")

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[len(headers)-1],
		"status=cancelled samples_expected=12 samples_written=3",
		"the closing block is written whatever stopped the window")
}

func TestWindowDeadlineExceededIsNotCancelled(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	window.Duration = 10 * time.Millisecond
	window.grace = 200 * time.Millisecond

	window.after = func(d time.Duration) <-chan time.Time { return time.After(time.Minute) }

	results := window.Run(context.Background())

	assert.Equal(t, StatusDeadlineExceeded, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)

	headers := headersOf(t, results[0])
	assert.Contains(t, headers[len(headers)-1],
		"status=deadline_exceeded samples_expected=2 samples_written=1")
}

func TestWindowWritesTheClosingBlockWhenTheContextIsAlreadyDone(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	results := newTestWindow(t, clock, collector).Run(ctx)

	assert.Equal(t, StatusCancelled, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)

	headers := headersOf(t, results[0])
	require.Len(t, headers, 2)
	assert.Contains(t, headers[1], "status=cancelled samples_expected=2 samples_written=0")
}

func TestWindowModuleDeadlineIsTheWindowPlusGrace(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var connectDeadline time.Time
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		connectDeadline, _ = ctx.Deadline()
		return newFakeWindowConn(), nil
	}

	before := time.Now()
	window.Run(context.Background())
	after := time.Now()

	require.NotEmpty(t, collector.deadlines)
	assert.WithinRange(t, collector.deadlines[0],
		before.Add(window.Duration+WindowGrace), after.Add(window.Duration+WindowGrace),
		"the module deadline is the window plus the grace")

	assert.WithinRange(t, connectDeadline,
		before.Add(ConnectTimeout), after.Add(ConnectTimeout))
}

func TestWindowModuleDeadlineExcludesTheConnect(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var connectReturned time.Time
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		time.Sleep(100 * time.Millisecond)
		connectReturned = time.Now()

		return newFakeWindowConn(), nil
	}

	window.Run(context.Background())

	require.NotEmpty(t, collector.deadlines)
	assert.False(t, collector.deadlines[0].Before(connectReturned.Add(window.Duration+WindowGrace)),
		"the deadline was armed before connecting, spending grace the final sample needs")
}

func TestWindowWritesTheStubBlockForAFailedSample(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			return errors.New("ERROR: canceling statement due to statement timeout")
		}
		return writeRows(w, []string{"relid"}, [][]string{{"16390"}})
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten, "only complete samples are counted")
	assert.Len(t, collector.seen, 2, "a failed sample does not stop the window")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 3, "preamble, the stub, and the closing block")

	assert.Contains(t, headers[1], "source=pg_fake",
		"the stub names the artifact, not the view: it is about the sampling")
	assert.Contains(t, headers[1],
		`sample=1 sample_error="ERROR: canceling statement due to statement timeout"`,
		"driver text is quoted, so it cannot break k=v tokenisation")

	assert.Contains(t, headers[2], "status=partial samples_expected=2 samples_written=1")
}

func TestWindowRedactsThePasswordFromASampleError(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_fake")
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		return fmt.Errorf("connection string was postgres://u:%s@h/db", testWindowTarget().Password)
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.NotContains(t, artifactText(t, results[0]), testWindowTarget().Password)
	assert.NotContains(t, results[0].Err, testWindowTarget().Password)
	assert.Contains(t, artifactText(t, results[0]), "<redacted>")
}

func TestWindowCreateFailureIsRecordedAndIsolated(t *testing.T) {
	clock := newFakeClock()

	blocked := newFakeCollector("pg_blocked")
	healthy := newFakeCollector("pg_healthy")

	window := newTestWindow(t, clock, blocked, healthy)

	require.NoError(t, os.Mkdir(blocked.artifact.FileName, 0o755))

	results := window.Run(context.Background())

	require.Len(t, results, 2)

	assert.Error(t, results[0].IOErr, "the one failure the artifact cannot record about itself")
	assert.Nil(t, results[0].File)
	assert.Equal(t, 0, results[0].SamplesWritten)
	assert.Empty(t, blocked.seen, "a collector with nowhere to write is not asked to sample")

	assert.NoError(t, results[1].IOErr, "one artifact's I/O failure does not affect another's")
	assert.Equal(t, StatusComplete, results[1].Status)
	assert.Equal(t, 2, results[1].SamplesWritten)
}

func TestWindowRunsEveryCollectorIntoItsOwnFile(t *testing.T) {
	clock := newFakeClock()

	first := newFakeCollector("pg_first")
	second := newFakeCollector("pg_second")

	results := newTestWindow(t, clock, first, second).Run(context.Background())

	require.Len(t, results, 2)
	for _, result := range results {
		assert.Equal(t, StatusComplete, result.Status)
		assert.Equal(t, 2, result.SamplesWritten)
		assert.Len(t, headersOf(t, result), 4)
	}

	assert.Contains(t, artifactText(t, results[0]), "source=pg_first")
	assert.Contains(t, artifactText(t, results[1]), "source=pg_second")
	assert.NotContains(t, artifactText(t, results[0]), "pg_second")
}

func TestWindowOneCollectorsFailureDoesNotAffectAnother(t *testing.T) {
	clock := newFakeClock()

	failing := newFakeCollector("pg_failing")
	failing.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		return errors.New("ERROR: permission denied for view pg_stat_user_tables")
	}
	healthy := newFakeCollector("pg_healthy")

	results := newTestWindow(t, clock, failing, healthy).Run(context.Background())

	assert.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 0, results[0].SamplesWritten)

	assert.Equal(t, StatusComplete, results[1].Status)
	assert.Equal(t, 2, results[1].SamplesWritten)
}

func TestWindowFallsBackToTheConfiguredDatabaseWhenIdentifyFails(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		conn := newFakeWindowConn()
		conn.identifyErr = errors.New("ERROR: permission denied")

		return conn, nil
	}

	results := window.Run(context.Background())

	assert.Equal(t, StatusComplete, results[0].Status,
		"an unidentified database is still a captured one")
	assert.Equal(t, "orders_configured", collector.seen[0].Database)
	assert.Empty(t, collector.seen[0].DBID)
}

func TestWindowClosesTheConnection(t *testing.T) {
	clock := newFakeClock()
	collector := newFakeCollector("pg_fake")

	conn := newFakeWindowConn()

	window := newTestWindow(t, clock, collector)
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		return conn, nil
	}

	window.Run(context.Background())

	assert.True(t, conn.closed, "the window returns its slot")
}

func TestWindowWithNoCollectorsIsHarmless(t *testing.T) {
	clock := newFakeClock()

	assert.Empty(t, newTestWindow(t, clock).Run(context.Background()))
}

func TestStoppedStatusDistinguishesTheDeadlineFromTheCaller(t *testing.T) {
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	assert.Equal(t, StatusDeadlineExceeded, stoppedStatus(expired))

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, StatusCancelled, stoppedStatus(cancelled))
}

func TestWindowSecondsRendersForAMachineReader(t *testing.T) {
	assert.Equal(t, "120s", windowSeconds(120*time.Second))
	assert.Equal(t, "600s", windowSeconds(600*time.Second))
	assert.Equal(t, "10s", windowSeconds(10*time.Second))
	assert.Equal(t, "90.5s", windowSeconds(90500*time.Millisecond),
		"a window that is not whole seconds keeps its precision")
}

func TestScheduleOffsets(t *testing.T) {
	const (
		s  = time.Second
		ms = time.Millisecond
	)

	for _, tc := range []struct {
		name     string
		schedule Schedule
		window   time.Duration
		want     []time.Duration
	}{
		{
			name:     "start and end sample the window's two edges",
			schedule: StartEnd(), window: 120 * s,
			want: []time.Duration{0, 120 * s},
		},
		{
			name:     "every 10s over the default window is twelve samples, the last at t0+110s",
			schedule: Every(10 * s), window: 120 * s,
			want: []time.Duration{
				0, 10 * s, 20 * s, 30 * s, 40 * s, 50 * s,
				60 * s, 70 * s, 80 * s, 90 * s, 100 * s, 110 * s,
			},
		},
		{
			name:     "once samples as the window opens and never at its close",
			schedule: Once(), window: 120 * s,
			want: []time.Duration{0},
		},
		{
			name:     "once is one sample however short the window",
			schedule: Once(), window: 1,
			want: []time.Duration{0},
		},
		{
			name:     "once on a zero window is still one sample",
			schedule: Once(), window: 0,
			want: []time.Duration{0},
		},
		{
			name:     "a window the interval does not divide keeps the strict inequality",
			schedule: Every(10 * s), window: 25 * s,
			want: []time.Duration{0, 10 * s, 20 * s},
		},
		{
			name:     "a window shorter than the interval yields exactly one sample",
			schedule: Every(10 * s), window: 5 * s,
			want: []time.Duration{0},
		},
		{
			name:     "a zero interval samples once rather than never",
			schedule: Every(0), window: 120 * s,
			want: []time.Duration{0},
		},
		{
			name:     "a negative interval samples once rather than never",
			schedule: Every(-time.Minute), window: 120 * s,
			want: []time.Duration{0},
		},
		{
			name:     "a zero window samples once, on either schedule",
			schedule: StartEnd(), window: 0,
			want: []time.Duration{0},
		},
		{
			name:     "a zero window samples once, on either cadence",
			schedule: Every(10 * s), window: 0,
			want: []time.Duration{0},
		},
		{
			name:     "a negative window samples once",
			schedule: StartEnd(), window: -time.Minute,
			want: []time.Duration{0},
		},
		{
			name:     "sub-second cadences are the same arithmetic",
			schedule: Every(500 * ms), window: 2 * s,
			want: []time.Duration{0, 500 * ms, s, 1500 * ms},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.schedule.offsets(tc.window))
		})
	}
}

func TestScheduleRendersItselfForThePreamble(t *testing.T) {
	assert.Equal(t, "start_end", StartEnd().name())
	assert.Empty(t, StartEnd().intervalText(), "a start-and-end schedule has no cadence to state")

	assert.Equal(t, "every", Every(10*time.Second).name())
	assert.Equal(t, "10s", Every(10*time.Second).intervalText())
	assert.Equal(t, "2s", Every(2*time.Second).intervalText())
	assert.Equal(t, "0.5s", Every(500*time.Millisecond).intervalText())

	assert.Empty(t, Every(0).intervalText(), "a degenerate interval states no cadence either")

	assert.Equal(t, "once", Once().name())
	assert.Empty(t, Once().intervalText(), "one reading has no cadence to state")
}

func TestTimelineMergesTwoCadencesInOffsetOrder(t *testing.T) {
	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(10 * time.Second)

	edges := newFakeCollector("pg_edges")

	events := timeline([]Collector{interval, edges}, 30*time.Second)

	assert.Equal(t, []sampleEvent{
		{at: 0, collector: 0, index: 1},
		{at: 0, collector: 1, index: 1},
		{at: 10 * time.Second, collector: 0, index: 2},
		{at: 20 * time.Second, collector: 0, index: 3},
		{at: 30 * time.Second, collector: 1, index: 2},
	}, events)
}

func TestTimelineBreaksTiesOnRegistrationOrder(t *testing.T) {
	edges := newFakeCollector("pg_edges")

	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(10 * time.Second)

	events := timeline([]Collector{edges, interval}, 30*time.Second)

	require.Len(t, events, 5)
	assert.Equal(t, sampleEvent{at: 0, collector: 0, index: 1}, events[0],
		"the shared tick runs the collectors in the order they were registered")
	assert.Equal(t, sampleEvent{at: 0, collector: 1, index: 1}, events[1])
}

func TestWindowRunsTwoCadencesOnOneConnection(t *testing.T) {
	clock := newFakeClock()

	var order []string
	record := func(name string) func(context.Context, SampleContext, io.Writer) error {
		return func(ctx context.Context, s SampleContext, w io.Writer) error {
			order = append(order, fmt.Sprintf("%s#%d@%s", name, s.Index, s.At.Sub(testWindowStart)))
			return nil
		}
	}

	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(10 * time.Second)
	interval.sample = record("interval")

	edges := newFakeCollector("pg_edges")
	edges.sample = record("edges")

	results := newTestWindow(t, clock, interval, edges).Run(context.Background())

	require.Len(t, results, 2)
	assert.Equal(t, 12, results[0].SamplesExpected, "twelve samples at 10s over 120s")
	assert.Equal(t, 12, results[0].SamplesWritten)
	assert.Equal(t, StatusComplete, results[0].Status)

	assert.Equal(t, 2, results[1].SamplesExpected)
	assert.Equal(t, 2, results[1].SamplesWritten)
	assert.Equal(t, StatusComplete, results[1].Status)

	require.Len(t, order, 14)
	assert.Equal(t, []string{"interval#1@0s", "edges#1@0s"}, order[:2],
		"the one shared tick runs both, cheap collector first")
	assert.Equal(t, []string{
		"interval#2@10s", "interval#3@20s", "interval#4@30s",
		"interval#5@40s", "interval#6@50s", "interval#7@1m0s",
		"interval#8@1m10s", "interval#9@1m20s", "interval#10@1m30s",
		"interval#11@1m40s", "interval#12@1m50s",
	}, order[2:13], "every intermediate tick is the interval collector's alone")
	assert.Equal(t, "edges#2@2m0s", order[13],
		"the closing tick is the start-and-end collector's alone - what WindowGrace is sized for")
}

func TestWindowOnceSamplesAtTheStartAndNotAtTheClosingTick(t *testing.T) {
	clock := newFakeClock()

	capability := newFakeCollector("pg_once")
	capability.artifact.Schedule = Once()

	edges := newFakeCollector("pg_edges")

	results := newTestWindow(t, clock, capability, edges).Run(context.Background())

	require.Len(t, results, 2)

	assert.Equal(t, 1, results[0].SamplesExpected, "a capability read is one reading")
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, StatusComplete, results[0].Status)

	require.Len(t, capability.seen, 1)
	assert.Equal(t, testWindowStart, capability.seen[0].At, "and it is taken as the window opens")

	// The invariant WindowGrace is sized on, re-asserted against the new kind
	// rather than assumed to have survived it.
	events := timeline([]Collector{capability, edges}, 120*time.Second)
	require.NotEmpty(t, events)
	assert.Equal(t, sampleEvent{at: 120 * time.Second, collector: 1, index: 2}, events[len(events)-1],
		"the closing tick is still the start-and-end collector's alone")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 3, "preamble, one sample, closing block")
	assert.Contains(t, headers[0],
		"status=started window=120s schedule=once interval= samples_expected=1")
	assert.Contains(t, headers[2], "status=complete samples_expected=1 samples_written=1")
}

func TestWindowRunsASharedTickInRegistrationOrder(t *testing.T) {
	capability := newFakeCollector("pg_once")
	capability.artifact.Schedule = Once()

	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(10 * time.Second)

	edges := newFakeCollector("pg_edges")

	events := timeline([]Collector{interval, capability, edges}, 30*time.Second)

	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, []sampleEvent{
		{at: 0, collector: 0, index: 1},
		{at: 0, collector: 1, index: 1},
		{at: 0, collector: 2, index: 1},
	}, events[:3], "three collectors share t0 and run in the order they were registered")
}

func TestWindowWritesThePrologueBeforeConnecting(t *testing.T) {
	clock := newFakeClock()
	collector := newFakePrologueCollector("pg_fake")

	window := newTestWindow(t, clock, collector)

	var atConnect string
	window.connect = func(ctx context.Context, target Target) (windowConn, error) {
		content, err := os.ReadFile(collector.artifact.FileName)
		require.NoError(t, err, "the artifact must exist before the connection is attempted")
		atConnect = string(content)

		return nil, ErrTooManyConnections
	}

	results := window.Run(context.Background())

	assert.Contains(t, atConnect, "status=started",
		"the preamble must be on disk, and synced, before Connect")
	assert.Contains(t, atConnect, "source=pg_fake_target",
		"and so must the prologue: it is what the artifact says on every failure path")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 3, "preamble, prologue, closing block - and no sample")
	assert.Contains(t, headers[2], "status=connect_failed")
}

func TestWindowPrologueCarriesTheWindowsClockNotTheZeroTime(t *testing.T) {
	clock := newFakeClock()
	collector := newFakePrologueCollector("pg_fake")

	results := newTestWindow(t, clock, collector).Run(context.Background())

	require.Len(t, collector.seenPrologue, 1)
	assert.Equal(t, testWindowStart, collector.seenPrologue[0].At,
		"a zero At would date every prologue block to year one")
	assert.Zero(t, collector.seenPrologue[0].Index, "there is no sample yet")
	assert.Equal(t, "orders_configured", collector.seenPrologue[0].Database,
		"and no connection, so the database is the configured name and there is no OID")
	assert.Empty(t, collector.seenPrologue[0].DBID)

	assert.Contains(t, headersOf(t, results[0])[1], "ts="+timestamp(testWindowStart))
}

func TestWindowDoesNotAskACollectorWithoutAPrologue(t *testing.T) {
	clock := newFakeClock()

	plain := newFakeCollector("pg_plain")
	withPrologue := newFakePrologueCollector("pg_prologue")

	results := newTestWindow(t, clock, plain, withPrologue).Run(context.Background())

	assert.Len(t, headersOf(t, results[0]), 4,
		"preamble, two samples, closing block - a collector that implements nothing is not asked")
	assert.NotContains(t, artifactText(t, results[0]), "_target")

	assert.Len(t, headersOf(t, results[1]), 5, "and the one that does gets its block")
}

func TestWindowPrologueFailureIsIsolated(t *testing.T) {
	clock := newFakeClock()

	failing := newFakePrologueCollector("pg_failing")
	failing.prologue = func(io.Writer, SampleContext) error {
		return errors.New("no space left on device")
	}

	healthy := newFakeCollector("pg_healthy")

	results := newTestWindow(t, clock, failing, healthy).Run(context.Background())

	require.Error(t, results[0].IOErr, "a prologue failure is an I/O failure on that artifact")
	assert.Empty(t, failing.seen, "a collector with nowhere to write is not asked to sample")

	assert.NoError(t, results[1].IOErr, "and it costs the other artifacts nothing")
	assert.Equal(t, StatusComplete, results[1].Status)
	assert.Equal(t, 2, results[1].SamplesWritten)
}

func TestWindowIntervalSampleNumberingIsPerArtifact(t *testing.T) {
	clock := newFakeClock()

	interval := newFakeCollector("pg_interval")
	interval.artifact.Schedule = Every(60 * time.Second)

	edges := newFakeCollector("pg_edges")

	results := newTestWindow(t, clock, interval, edges).Run(context.Background())

	assert.Equal(t, []int{1, 2}, sampleIndexes(interval.seen))
	assert.Equal(t, []int{1, 2}, sampleIndexes(edges.seen),
		"both artifacts number from 1: pg_interval's sample=2 and pg_edges's are unrelated events")

	assert.Equal(t, testWindowStart.Add(60*time.Second), interval.seen[1].At)
	assert.Equal(t, testWindowStart.Add(120*time.Second), edges.seen[1].At)

	assert.Contains(t, headersOf(t, results[0])[2], "sample=2")
	assert.Contains(t, headersOf(t, results[1])[2], "sample=2")
}

func sampleIndexes(seen []SampleContext) []int {
	indexes := make([]int, len(seen))
	for i, s := range seen {
		indexes[i] = s.Index
	}

	return indexes
}

func TestWindowIntervalTicksAreAbsoluteNotRelativeToTheLastSample(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_interval")
	collector.artifact.Schedule = Every(10 * time.Second)
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(4 * time.Second)
		}
		return nil
	}

	newTestWindow(t, clock, collector).Run(context.Background())

	require.Len(t, collector.seen, 12)
	assert.Equal(t, testWindowStart.Add(10*time.Second), collector.seen[1].At,
		"sample 1's latency is absorbed, not added to every later tick")
	assert.Equal(t, testWindowStart.Add(110*time.Second), collector.seen[11].At)

	assert.Equal(t, 6*time.Second, clock.waits[0], "the wait is the offset minus what elapsed")
}

func TestWindowOverdueTicksFireImmediatelyRatherThanBeingSkipped(t *testing.T) {
	clock := newFakeClock()

	collector := newFakeCollector("pg_interval")
	collector.artifact.Schedule = Every(10 * time.Second)
	collector.sample = func(ctx context.Context, s SampleContext, w io.Writer) error {
		if s.Index == 1 {
			clock.advance(25 * time.Second)
		}
		return nil
	}

	results := newTestWindow(t, clock, collector).Run(context.Background())

	assert.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 12, results[0].SamplesWritten,
		"a slow sample costs cadence, never a sample: samples_written still reaches samples_expected")

	require.Len(t, collector.seen, 12)
	assert.Equal(t, testWindowStart.Add(25*time.Second), collector.seen[1].At,
		"the tick due at t0+10s was overdue and fired at once")
	assert.Equal(t, testWindowStart.Add(25*time.Second), collector.seen[2].At,
		"so did the one due at t0+20s - two blocks with near-identical ts=, which is a catch-up burst")
	assert.Equal(t, testWindowStart.Add(30*time.Second), collector.seen[3].At,
		"and then the cadence resumes, because offsets are absolute")
}
