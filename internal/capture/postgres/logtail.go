package postgres

import (
	"bytes"
	"cmp"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Shared engine for pg_deadlocks.txt/pg_timeouts.txt/pg_checkpoint_log.txt; only the matcher differs. Body is raw bytes, not per-row.
// log_line_prefix may start with '#' and csvlog DETAIL fields hold real newlines, so no scan finds a terminator — bytes= marks the end.
const (
	// DefaultLogTailInterval is the poll cadence. Log is append-only (rotation renames, not truncates) so polling loses nothing, unlike pg_stat_activity's missed-sample loss.
	// 10s, not 60s: bounds how much evidence a cancelled window loses.
	DefaultLogTailInterval = 10 * time.Second

	// LogDrainBudget bounds WriteClosing's drain, checked between reads since os.File.Read takes no context (a hung read can still exceed it).
	// Four Closing implementations add up to 4x this beyond moduleDeadline, though pg_explain.txt's fires only where the closing sample never ran.
	LogDrainBudget = 5 * time.Second

	// MaxScanBytes is the per-sample read cap. Beyond it the tail seeks to EOF and records scan_truncated=true skipped_bytes=<n> instead of matched=0.
	// A deadlock may have occurred in the skipped gap.
	MaxScanBytes = 16 << 20

	// MaxArtifactBytes bounds everything this collector writes (headers + bodies) across all samples and the drain.
	// The window's own preamble/closing block are separate.
	MaxArtifactBytes = 32 << 20

	// MaxEventBytes bounds one copied event, truncation mark counted inside the cap.
	// Byte-based, not truncateRunes: that rewrites invalid UTF-8 to U+FFFD and counts runes against a byte cap.
	MaxEventBytes = 256 << 10

	// MaxEventLines caps an event with no provable end, so it can't swallow the rest
	// of the file into one block.
	MaxEventLines = 200
)

// eventTruncationMark is counted inside MaxEventBytes rather than added to it.
const eventTruncationMark = "..."

// headFingerprint is bytes kept from offset 0 to detect a file truncated and regrown
// past the saved offset — invisible to a size check alone.
const headFingerprint = 64

// logFormat identifies which of PostgreSQL's three log encodings the body uses.
type logFormat string

const (
	logFormatJSON   logFormat = "jsonlog"
	logFormatCSV    logFormat = "csvlog"
	logFormatStderr logFormat = "stderr"
)

// logFormatPreference is most-structured first: SQLSTATE beats a translatable message, one line beats a record spanning physical lines.
// jsonlog requires PostgreSQL 15+; destinations are read from the server, not assumed.
var logFormatPreference = []logFormat{logFormatJSON, logFormatCSV, logFormatStderr}

// reason* explain why there is no log to read; never paired with matched=, so
// absence never renders as a measured 0.
const (
	// reasonCollectorOff: logging_collector is off, so the server writes no file.
	reasonCollectorOff = "collector_off"

	// reasonUnresolved: no route produced a path.
	reasonUnresolved = "unresolved"

	// reasonUnreadable: a path was found but this process cannot open it — the
	// default outcome on a default install, where log files are 0600 owned by postgres.
	reasonUnreadable = "unreadable"

	// reasonSettingsUnread: resolution could not run at all - the settings read
	// failed and no route answered.
	reasonSettingsUnread = "settings_unread"
)

// How the source was found, written into every block as log_resolved_by= so a
// guess is never read as a fact.
const (
	resolvedByCurrentLogfiles = "current_logfiles"
	resolvedByFunction        = "pg_current_logfile"
	resolvedByGlob            = "glob"
)

// matchedBy is how events were recognised; on stderr always message.
// SQLSTATE-via-%e was dropped: the one path where a match could succeed while the boundary rule ran blind.
const (
	matchedBySQLState = "sqlstate"
	matchedByMessage  = "message"
)

// logSettings holds what log resolution needs, read on the tail's first sample.
// data_directory/log_directory/log_filename are superuser-only GUCs; pg_settings returns NULL (not an error) for them to a lesser role, hence reason=unresolved not a failure.
type logSettings struct {
	dataDirectory    string
	logDirectory     string
	logFilename      string
	loggingCollector string
	logDestination   string

	// logLinePrefix is the stderr format's line prefix template. pg_explain.txt reads
	// it for %Q, the one place a stderr entry carries a query identifier; the other
	// tails ignore it.
	logLinePrefix string

	// serverAddr is host(inet_server_addr()); empty means a unix socket, which is
	// itself proof the server is this host.
	serverAddr string

	// read is false when the statement behind these failed, which is the
	// difference between reason=unresolved and reason=settings_unread.
	read bool
}

// logSettingsSQL: scalar subqueries over pg_settings, not SHOW/current_setting() —
// those raise for a name this role can't see, where pg_settings returns NULL instead.
const logSettingsSQL = `SELECT
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'data_directory'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'log_directory'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'log_filename'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'logging_collector'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'log_destination'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'log_line_prefix'),
    host(inet_server_addr())`

// logLocationFormatSQL always names a format: the no-arg pg_current_logfile()
// prefers stderr first, the inverse of the matcher's preference order.
const logLocationFormatSQL = `SELECT pg_current_logfile($1)`

func readLogSettings(ctx context.Context, q Querier) (logSettings, error) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var dataDirectory, logDirectory, logFilename, loggingCollector, logDestination,
		logLinePrefix, serverAddr *string

	err := q.QueryRow(ctx, logSettingsSQL).Scan(
		&dataDirectory, &logDirectory, &logFilename, &loggingCollector, &logDestination,
		&logLinePrefix, &serverAddr)
	if err != nil {
		return logSettings{}, err
	}

	return logSettings{
		dataDirectory:    text(dataDirectory),
		logDirectory:     text(logDirectory),
		logFilename:      text(logFilename),
		loggingCollector: text(loggingCollector),
		logDestination:   text(logDestination),
		logLinePrefix:    text(logLinePrefix),
		serverAddr:       text(serverAddr),
		read:             true,
	}, nil
}

// logSource is the resolved file (path, format, how found).
// Success is cached; failure is retried each sample, so a transient failure costs one block, not the run.
type logSource struct {
	// path is absolute and in this process's namespace; raw is what the winning
	// route named, which may be relative to the data directory.
	path string
	raw  string

	format     logFormat
	resolvedBy string

	// available is every destination the cluster writes, so a bundle says what
	// it could have read as well as what it did.
	available []logFormat

	// reason is why there is no readable source. Never written beside matched=.
	reason string

	// err is the last route's SQL error, already redacted (not necessarily the
	// only failure).
	err string
}

func (s logSource) formatNames() string {
	names := make([]string, len(s.available))
	for i, f := range s.available {
		names[i] = string(f)
	}

	return strings.Join(names, ",")
}

// logAccess reports the result of the open test; logAccessReason says why it is
// not direct. Unknown means the test could not run at all — not a denial, which
// maps to none with its own reason.
func (s logSource) logAccess() string {
	switch s.reason {
	case "":
		return LogAccessDirect

	case reasonSettingsUnread:
		return LogAccessUnknown
	}

	return LogAccessNone
}

// logAccessReason is empty exactly when logAccess is direct: a reason is mandatory
// whenever the fact is not the decided one.
func (s logSource) logAccessReason() string { return s.reason }

// matchedBy is how this tail's events were recognised: by message on stderr,
// where no code is available, and wherever the matcher's every code is paired
// with its message - a LOG line's 00000 names nothing, so the message decided.
func (t *logTail) matchedBy() string {
	if t.source.format == logFormatStderr || t.match.messageDecides() {
		return matchedByMessage
	}

	return matchedBySQLState
}

// Three routes in order. pg_current_logfile() is denied to pg_monitor on PG14-16 (EXECUTE grant landed in 17); current_logfiles needs only data_directory.
// Not redundant: a hardened deployment can expose log_directory but not data_directory, where only the function route finds the current file.
func resolveLogSource(ctx context.Context, q Querier, s logSettings, redact func(error) string) logSource {
	var src logSource

	// No route needed: collector off means there is no file.
	if s.read && strings.EqualFold(s.loggingCollector, "off") {
		src.reason = reasonCollectorOff
		return src
	}

	if raw, format, available, ok := resolveFromCurrentLogfiles(s); ok {
		src.raw, src.format, src.available, src.resolvedBy = raw, format, available, resolvedByCurrentLogfiles
		return finishLogSource(src, s)
	}

	raw, format, available, err, ok := resolveFromFunction(ctx, q, s)
	if err != nil && redact != nil {
		src.err = redact(err)
	}
	if ok {
		src.raw, src.format, src.available, src.resolvedBy = raw, format, available, resolvedByFunction

		// Relative path with no data_directory to resolve against: the route didn't
		// conclude, but raw is kept as what the server said.
		if resolved := finishLogSource(src, s); resolved.path != "" {
			return resolved
		}

		src.resolvedBy = ""
		src.format = ""
	}

	if raw, format, available, ok := resolveFromGlob(s); ok {
		src.raw, src.format, src.available, src.resolvedBy = raw, format, available, resolvedByGlob
		return finishLogSource(src, s)
	}

	// No route succeeds for a bare LOGIN role on any supported version (all three
	// GUCs are superuser-only, the function needs EXECUTE) — this is where that lands.
	src.reason = reasonUnresolved
	if !s.read {
		src.reason = reasonSettingsUnread
	}

	return src
}

// finishLogSource resolves the route's raw answer to a path and tests only that the file opens, not directory listing.
// A hardened 0711 dir may deny listing while the file inside still opens; glob is the one route needing directory read.
func finishLogSource(src logSource, s logSettings) logSource {
	resolved, ok := resolveLogfile(src.raw, s.dataDirectory)
	if !ok {
		return src
	}

	src.path = resolved

	if !isReadable(resolved) {
		src.reason = reasonUnreadable
	}

	return src
}

// resolveFromCurrentLogfiles reads PostgreSQL's <data_directory>/current_logfiles: one line per destination, "<format> <path>" relative to the data directory.
// Re-read every sample (how rotation is noticed); a malformed line is skipped, not fatal.
func resolveFromCurrentLogfiles(s logSettings) (raw string, format logFormat, available []logFormat, ok bool) {
	if s.dataDirectory == "" {
		return "", "", nil, false
	}

	content, err := os.ReadFile(filepath.Join(s.dataDirectory, "current_logfiles"))
	if err != nil {
		return "", "", nil, false
	}

	paths := map[logFormat]string{}

	for line := range strings.SplitSeq(string(content), "\n") {
		name, path, found := strings.Cut(strings.TrimSpace(line), " ")
		if !found {
			continue
		}

		f := logFormat(name)
		if !slices.Contains(logFormatPreference, f) {
			continue
		}

		paths[f] = strings.TrimSpace(path)
	}

	for _, f := range logFormatPreference {
		if _, held := paths[f]; held {
			available = append(available, f)
		}
	}

	if len(available) == 0 {
		return "", "", nil, false
	}

	return paths[available[0]], available[0], available, true
}

// resolveFromFunction asks pg_current_logfile per destination in preference order,
// falling back to the no-arg form only when log_destination lists nothing usable.
func resolveFromFunction(ctx context.Context, q Querier, s logSettings) (
	raw string, format logFormat, available []logFormat, err error, ok bool,
) {
	available = destinationFormats(s.logDestination)

	for _, f := range available {
		path, callErr := currentLogfile(ctx, q, string(f))
		if callErr != nil {
			err = callErr
			continue
		}

		if path != "" {
			return path, f, available, err, true
		}
	}

	if len(available) > 0 {
		return "", "", available, err, false
	}

	path, callErr := currentLogfile(ctx, q, "")
	if callErr != nil {
		return "", "", nil, callErr, false
	}

	if path == "" {
		return "", "", nil, err, false
	}

	format = formatFromExtension(path)

	return path, format, []logFormat{format}, err, true
}

func currentLogfile(ctx context.Context, q Querier, format string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var logfile *string

	if format == "" {
		if err := q.QueryRow(ctx, logLocationSQL).Scan(&logfile); err != nil {
			return "", err
		}

		return text(logfile), nil
	}

	if err := q.QueryRow(ctx, logLocationFormatSQL, format).Scan(&logfile); err != nil {
		return "", err
	}

	return text(logfile), nil
}

// resolveFromGlob composes log_directory + log_filename (may carry strftime escapes, e.g. %Y-%m-%d_%H%M%S) and takes the newest match — a guess at the rotation.
// Extension substituted first: log_filename names only the stderr file; PostgreSQL swaps trailing .log for .csv/.json (or appends), else glob tails the wrong file on a csvlog-only cluster.
// The one route needing directory read (listing); others only traverse.
func resolveFromGlob(s logSettings) (raw string, format logFormat, available []logFormat, ok bool) {
	if s.logFilename == "" {
		return "", "", nil, false
	}

	dir := s.logDirectory
	if dir == "" {
		return "", "", nil, false
	}

	if !isAbsolutePath(dir) {
		if s.dataDirectory == "" {
			return "", "", nil, false
		}

		dir = filepath.Join(s.dataDirectory, dir)
	}

	available = destinationFormats(s.logDestination)
	if len(available) == 0 {
		available = logFormatPreference
	}

	for _, f := range available {
		pattern := filepath.Join(dir, globPattern(substituteExtension(s.logFilename, f)))

		if newest, found := newestMatch(pattern); found {
			return newest, f, available, true
		}
	}

	return "", "", available, false
}

func newestMatch(pattern string) (string, bool) {
	matches, err := filepath.Glob(pattern)
	if err != nil || len(matches) == 0 {
		return "", false
	}

	type candidate struct {
		path string
		at   time.Time
	}

	var candidates []candidate
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			continue
		}

		candidates = append(candidates, candidate{path: match, at: info.ModTime()})
	}

	if len(candidates) == 0 {
		return "", false
	}

	// Name is the tie-break: same-tick mtimes would otherwise resolve differently
	// between samples, read as a spurious rotation.
	slices.SortFunc(candidates, func(a, b candidate) int {
		if order := b.at.Compare(a.at); order != 0 {
			return order
		}

		return cmp.Compare(a.path, b.path)
	})

	return candidates[0].path, true
}

// globPattern replaces strftime escapes with '*' (collapsed) — the guess is exactly
// which rotation this matches.
func globPattern(filename string) string {
	var out strings.Builder

	for i := 0; i < len(filename); i++ {
		if filename[i] != '%' || i+1 >= len(filename) {
			// Escape glob metacharacters so a customer's log_filename can't silently
			// widen the pattern.
			if strings.ContainsRune(`*?[]\`, rune(filename[i])) {
				out.WriteByte('\\')
			}

			out.WriteByte(filename[i])

			continue
		}

		i++

		if !strings.HasSuffix(out.String(), "*") {
			out.WriteByte('*')
		}
	}

	return out.String()
}

// substituteExtension is PostgreSQL's own rule for the non-stderr destinations:
// a trailing .log is replaced, anything else is appended to.
func substituteExtension(filename string, format logFormat) string {
	var extension string

	switch format {
	case logFormatCSV:
		extension = ".csv"
	case logFormatJSON:
		extension = ".json"
	default:
		return filename
	}

	return strings.TrimSuffix(filename, ".log") + extension
}

func formatFromExtension(path string) logFormat {
	switch filepath.Ext(path) {
	case ".csv":
		return logFormatCSV
	case ".json":
		return logFormatJSON
	}

	return logFormatStderr
}

// destinationFormats narrows log_destination to formats this agent reads, in
// preference order; syslog/eventlog are dropped, not mapped.
func destinationFormats(logDestination string) []logFormat {
	var formats []logFormat

	for _, f := range logFormatPreference {
		for name := range strings.SplitSeq(logDestination, ",") {
			if logFormat(strings.TrimSpace(name)) == f {
				formats = append(formats, f)
				break
			}
		}
	}

	return formats
}

// eventMatch distinguishes the two artifacts: SQLSTATE match is exact and locale/version-independent.
// stderr matches on message text, which lc_messages translates — a known blind spot, not a workaround.
type eventMatch struct {
	sqlstate []string
	message  []string

	// paired lists codes ambiguous by SQLSTATE alone, matched on code+message when both are available.
	// 57014 (query_canceled) fires for both client cancel and pg_cancel_backend(); 55P03 (lock_not_available) fires for FOR UPDATE NOWAIT with no timeout.
	// On a non-English cluster these under-match rather than mis-attribute.
	paired []string

	// messageSuffix additionally requires the message's *first line* to end with one of these.
	// auto_explain and log_min_duration_statement both open "duration: <n> ms ", so only what follows tells a plan from a slow-query transcript.
	messageSuffix []string

	// messageContains is the other way past that requirement: a first line carrying one
	// of these is accepted too. pg_explain.txt's tail takes auto_explain's "plan:" entries
	// by suffix and log_min_duration_statement's "execute" records by this.
	messageContains []string
}

// messageDecides is true when no code alone can match: every SQLSTATE the
// matcher accepts is paired with its message, which is the shape a LOG-severity
// event has - 00000 is every LOG line's code.
func (m eventMatch) messageDecides() bool {
	if len(m.sqlstate) == 0 {
		return true
	}

	return !slices.ContainsFunc(m.sqlstate, func(code string) bool {
		return !slices.Contains(m.paired, code)
	})
}

func (m eventMatch) matches(sqlstate, message string) bool {
	if !slices.Contains(m.sqlstate, sqlstate) {
		return false
	}

	if !slices.Contains(m.paired, sqlstate) {
		return true
	}

	return m.matchesMessage(message)
}

func (m eventMatch) matchesMessage(s string) bool {
	if !slices.ContainsFunc(m.message, func(want string) bool { return strings.HasPrefix(s, want) }) {
		return false
	}

	if len(m.messageSuffix) == 0 && len(m.messageContains) == 0 {
		return true
	}

	// First line only: csvlog and jsonlog deliver the whole multi-line message here,
	// and every line after the first belongs to the plan body, not the predicate.
	first := s
	if at := strings.IndexAny(first, "\r\n"); at >= 0 {
		first = first[:at]
	}

	first = strings.TrimRight(first, " \t")

	return slices.ContainsFunc(m.messageSuffix, func(want string) bool {
		return strings.HasSuffix(first, want)
	}) || slices.ContainsFunc(m.messageContains, func(want string) bool {
		return strings.Contains(first, want)
	})
}

// severityKeywords open an entry, secondaryKeywords continue one. Both are English-only by design.
// PostgreSQL translates them (postgres:18: German ERROR->FEHLER, STATEMENT:->ANWEISUNG:; Russian translates all eight).
// Keeping English means a translated cluster fails the message match first, yielding clean matched=0 rather than a mis-bounded event.
var severityKeywords = []string{"DEBUG", "INFO", "NOTICE", "WARNING", "ERROR", "LOG", "FATAL", "PANIC"}

var secondaryKeywords = []string{"DETAIL", "HINT", "QUERY", "CONTEXT", "STATEMENT", "LOCATION"}

// keywordAt matches "KEYWORD:" and returns the rest with leading whitespace trimmed.
// PostgreSQL emits two spaces after the colon (confirmed in its translation catalogues); matching any run avoids matching nothing on real servers.
func keywordAt(s string, keywords []string) (rest string, ok bool) {
	for _, keyword := range keywords {
		if len(s) <= len(keyword) || s[len(keyword)] != ':' || s[:len(keyword)] != keyword {
			continue
		}

		return strings.TrimLeft(s[len(keyword)+1:], " \t"), true
	}

	return "", false
}

// keywordIndex finds the earliest keyword match by position, not list order — callers compare severity vs. secondary positions, only comparable that way.
// Bias is toward over-copying: an under-copied event loses evidence permanently.
func keywordIndex(line string, keywords []string) int {
	at := -1

	for _, keyword := range keywords {
		if i := strings.Index(line, keyword+":"); i >= 0 && (at < 0 || i < at) {
			at = i
		}
	}

	return at
}

// logTail is stateful across samples: file handle, offset, and a partial event
// held over from one Sample call to the next.
type logTail struct {
	// name is the artifact's source=, and scope is its scope=.
	name  string
	scope string

	match eventMatch

	settings     logSettings
	haveSettings bool

	source logSource

	// file != nil means resolved. A resolve/open failure retries next sample, so a
	// log file that becomes readable mid-window is picked up.
	file *os.File

	// offset is the read position in file. head is its first bytes at open,
	// re-verified each sample.
	offset int64
	head   []byte

	// lostPath/lostOffset remember a dropped handle (only rotation into an unreadable target causes this); lostPath == "" iff no handle has ever been held.
	// Any future code path that closes the handle mid-run must set both, or the next open will seek past missed bytes.
	lostPath   string
	lostOffset int64

	// pending holds a trailing incomplete unit (a read can end mid-line, or mid-record in csvlog where a quoted DETAIL field spans lines).
	// pendingIsEvent marks a matched stderr event whose end isn't provable yet — completed and copied on a later sample.
	pending        []byte
	pendingIsEvent bool

	// written counts everything this collector puts in the file, headers
	// included, against MaxArtifactBytes.
	written int64
	full    bool

	// announced is whether a block has already carried the source's identity.
	// log_path= is ~90 bytes and the file is otherwise a few hundred.
	announced bool
}

func newLogTail(name string, match eventMatch) logTail {
	return logTail{name: name, scope: "cluster", match: match}
}

// consumed excludes carried bytes, which belong to whichever block finally writes
// them — keeping from_offset/to_offset free of gaps and double-counting.
func (t *logTail) consumed() int64 { return t.offset - int64(len(t.pending)) }

// tailRead is one sample's worth of reading, and every header key that is a
// statement about it rather than about the source.
type tailRead struct {
	from int64
	to   int64

	// events are the matched events, each newline-terminated. Kept apart rather than
	// concatenated so a caller can attribute one at a time.
	events  [][]byte
	matched int

	rotated      bool
	previousPath string
	previousTo   int64

	// resolvedLate: from_offset is where coverage begins, not where the prior block ended — first resolution happened after sample 1.
	// t0..here was never read, unrecoverable since the file's size at t0 isn't knowable after the fact.
	resolvedLate bool

	truncated     bool
	scanTruncated bool
	skipped       int64

	carryDropped    bool
	eventsTruncated int
	partial         bool
}

func (r *tailRead) body() []byte { return bytes.Join(r.events, nil) }

// sample always writes exactly one block, even when there's nothing to read — a
// missing sample= would be an unexplained gap in the sequence.
func (t *logTail) sample(ctx context.Context, q Querier, w io.Writer, sc SampleContext) error {
	resolvedLate := t.resolveOnce(ctx, q, sc)

	if t.file == nil {
		return t.writeReasonBlock(w, sc)
	}

	read := &tailRead{from: t.consumed(), resolvedLate: resolvedLate}

	t.readOpenFile(read, time.Time{})
	t.followRotation(ctx, q, read)

	read.to = t.consumed()

	return t.writeReadBlock(w, sc, read, false)
}

// resolveOnce resolves and opens the source the first time it can and is a no-op after;
// a failure retries next call, so a log that becomes readable mid-window is still picked
// up. It reports whether that first open landed after sample 1, which makes from_offset a
// coverage start rather than a continuation.
func (t *logTail) resolveOnce(ctx context.Context, q Querier, sc SampleContext) (resolvedLate bool) {
	settingsErr := t.readSettings(ctx, q, sc)

	if t.file != nil {
		return false
	}

	source := resolveLogSource(ctx, q, t.settings, sc.errorText)

	// Settings-read failure wins over a route's error: it's the root cause, and
	// reporting a route's denial would misdirect the operator toward a grant issue.
	if settingsErr != "" {
		source.err = settingsErr
	}

	from := t.openFrom(source)

	if source.reason == "" {
		if err := t.open(source, from); err != nil {
			source.reason = reasonUnreadable
		} else {
			resolvedLate = from == openAtEOF && sc.Index > 1
		}
	}

	t.source = source
	t.announced = false

	return resolvedLate
}

// openAtEnd resolves and opens the log at its current end, writing nothing: the setup
// half for a collector that reads events back rather than transcribing them. Reports
// whether a handle was obtained; when false, t.source carries the reason= and
// log_access= the caller states instead of a count.
func (t *logTail) openAtEnd(ctx context.Context, q Querier, sc SampleContext) bool {
	t.resolveOnce(ctx, q, sc)

	return t.file != nil
}

// readEvents is sample()'s counterpart for a collector that attributes events rather than
// transcribing them: the matched events come back one slice each, leaving rendering, caps
// and counters to the caller. Final by construction - the pending bytes are flushed, so a held
// event comes back with read.partial set. A nil q skips rotation-following, which is how
// the closing pass reads: after cancellation there is no connection left to ask.
func (t *logTail) readEvents(ctx context.Context, q Querier, deadline time.Time) ([][]byte, *tailRead) {
	read := &tailRead{from: t.consumed()}

	if t.file == nil {
		return nil, read
	}

	t.readOpenFile(read, deadline)

	if q != nil {
		t.followRotation(ctx, q, read)
	}

	t.flushPending(read)

	read.to = t.consumed()

	return read.events, read
}

// writeClosing drains whatever grew after the last sample; otherwise the window's final interval goes unread under status=complete.
// Needs no connection or context — must still run after cancellation — so it never re-resolves; a rotation between the last sample and close leaves the new file unread (named residual).
// Closes the handle: the collector's last call.
func (t *logTail) writeClosing(w io.Writer, sc SampleContext) error {
	if t.file == nil {
		// Never resolved. The connect-failure path lands here, and the artifact
		// keeps its preamble-plus-closing-block shape.
		return nil
	}

	defer t.closeFile()

	read := &tailRead{from: t.consumed()}

	t.readOpenFile(read, time.Now().Add(LogDrainBudget))
	t.flushPending(read)

	read.to = t.consumed()

	return t.writeReadBlock(w, sc, read, true)
}

// writeBlock renders header+body into one buffer and issues a single Write.
// A failure mid-write would otherwise leave a bytes= promising content that never landed, making every later block unparseable.
func (t *logTail) writeBlock(w io.Writer, fields []headerField, body []byte, at time.Time) error {
	var block bytes.Buffer

	if err := writeBlockHeaderFormat(&block, t.name, t.scope, formatText, fields, at); err != nil {
		return err
	}

	block.Write(body)

	t.written += int64(block.Len())
	if t.written >= MaxArtifactBytes {
		t.full = true
	}

	_, err := w.Write(block.Bytes())

	return err
}

// writeReasonBlock deliberately omits matched=: matched=0 beside a
// reason would let a receiver sum/average/render it as a real zero measurement.
func (t *logTail) writeReasonBlock(w io.Writer, sc SampleContext) error {
	source := t.source

	fields := []headerField{
		{"db", sc.Database},
		{"dbid", sc.DBID},
		{"sample", strconv.Itoa(sc.Index)},
		{"reason", source.reason},
		{"log_access", source.logAccess()},
	}

	if source.resolvedBy != "" {
		fields = append(fields, headerField{"log_resolved_by", source.resolvedBy})
	}

	if source.path != "" {
		fields = append(fields, headerField{"log_path", source.path})
	}

	if source.reason == reasonUnreadable {
		fields = append(fields, headerField{"log_readable", "false"})
	}

	if source.err != "" {
		fields = append(fields, headerField{"error", source.err})
	}

	// bytes=0, not omitted: absent bytes= is the window's own preamble/closing-block
	// signature.
	fields = append(fields, headerField{"bytes", "0"})

	return t.writeBlock(w, fields, nil, sc.At)
}

// writeReadBlock: every conditional field is omitted by default and only appears
// when it changed (e.g. log_path=, ~90 bytes, is announced once, not every block).
func (t *logTail) writeReadBlock(w io.Writer, sc SampleContext, read *tailRead, drain bool) error {
	fields := []headerField{
		{"db", sc.Database},
		{"dbid", sc.DBID},
	}

	// drain=true rather than a sample number: keeps samples_written honest against
	// the schedule.
	if drain {
		fields = append(fields, headerField{"drain", "true"})
	} else {
		fields = append(fields, headerField{"sample", strconv.Itoa(sc.Index)})
	}

	fields = append(fields, headerField{"log_format", string(t.source.format)})

	if !t.announced {
		fields = append(fields, headerField{"log_formats", t.source.formatNames()})
	}

	if !t.announced || read.rotated {
		fields = append(fields, headerField{"log_path", t.source.path})
	}

	if read.rotated {
		fields = append(fields, headerField{"log_path_previous", read.previousPath})
	}

	fields = append(fields, headerField{"log_resolved_by", t.source.resolvedBy})

	if !t.announced {
		fields = append(fields, headerField{"log_access", t.source.logAccess()})
	}

	if !drain {
		fields = append(fields, headerField{"matched_by", t.matchedBy()})
	}

	fields = append(fields,
		headerField{"from_offset", strconv.FormatInt(read.from, 10)},
		headerField{"to_offset", strconv.FormatInt(read.to, 10)},
	)

	fields = append(fields, readStateFields(read)...)

	if t.full {
		fields = append(fields, headerField{"artifact_full", "true"})
	}

	body := read.body()

	fields = append(fields,
		headerField{"matched", strconv.Itoa(read.matched)},
		headerField{"bytes", strconv.Itoa(len(body))},
	)

	t.announced = true

	return t.writeBlock(w, fields, body, sc.At)
}

// readStateFields is everything a read says about itself rather than about the source.
// Shared with readEvents' callers: dropping them reports a read that overran MaxScanBytes
// - and therefore returned nothing - as an ordinary empty one.
func readStateFields(read *tailRead) []headerField {
	var fields []headerField

	if read.resolvedLate {
		fields = append(fields, headerField{"resolved_late", "true"})
	}

	if read.rotated {
		// rotated=true signals from/to_offset are in the new file; previous_to_offset
		// gives the superseded file's own end.
		fields = append(fields,
			headerField{"rotated", "true"},
			headerField{"previous_to_offset", strconv.FormatInt(read.previousTo, 10)},
		)
	}

	if read.truncated {
		fields = append(fields, headerField{"file_truncated", "true"})

		// Same rebase as rotation; only emitted if rotation didn't already state it.
		if !read.rotated {
			fields = append(fields,
				headerField{"previous_to_offset", strconv.FormatInt(read.previousTo, 10)})
		}
	}

	if read.scanTruncated {
		fields = append(fields,
			headerField{"scan_truncated", "true"},
			headerField{"skipped_bytes", strconv.FormatInt(read.skipped, 10)},
		)
	}

	if read.carryDropped {
		fields = append(fields, headerField{"carry_dropped", "true"})
	}

	if read.eventsTruncated > 0 {
		fields = append(fields, headerField{"events_truncated", strconv.Itoa(read.eventsTruncated)})
	}

	if read.partial {
		fields = append(fields, headerField{"partial_event", "true"})
	}

	return fields
}

// readSettings caches success only; a failure leaves settings.read false, which
// separates reason=unresolved from reason=settings_unread.
func (t *logTail) readSettings(ctx context.Context, q Querier, sc SampleContext) string {
	if t.haveSettings {
		return ""
	}

	settings, err := readLogSettings(ctx, q)
	if err != nil {
		return sc.errorText(err)
	}

	t.settings = settings
	t.haveSettings = true

	return ""
}

// openFrom is where a newly opened file is read from.
type openFrom int

const (
	// openAtEOF: first open of the run — seeks to EOF since events before t0 are
	// out of scope.
	openAtEOF openFrom = iota

	// openAtStart: a rotation target created during the window — read from 0, or
	// the gap between rotation and open is lost.
	openAtStart

	// openAtResume is the same file the tail already had a handle on and lost,
	// which continues where it left off: neither re-read nor skipped.
	openAtResume
)

func (t *logTail) openFrom(source logSource) openFrom {
	switch t.lostPath {
	case "":
		return openAtEOF

	case source.path:
		return openAtResume
	}

	return openAtStart
}

func (t *logTail) open(source logSource, from openFrom) error {
	file, err := os.Open(source.path)
	if err != nil {
		return err
	}

	t.file = file
	t.offset = 0
	t.head = nil
	t.pending = nil
	t.pendingIsEvent = false

	switch from {
	case openAtStart:
		// offset already 0: this file is entirely the window's.

	case openAtResume:
		t.offset = t.lostOffset

	default:
		// Seeks to EOF: see openAtEOF.
		if info, err := file.Stat(); err == nil {
			t.offset = info.Size()
		}
	}

	t.lostPath, t.lostOffset = "", 0

	t.head = t.readHead()

	return nil
}

func (t *logTail) closeFile() {
	if t.file == nil {
		return
	}

	t.file.Close()
	t.file = nil
}

func (t *logTail) readHead() []byte {
	head := make([]byte, headFingerprint)

	n, err := t.file.ReadAt(head, 0)
	if n == 0 && err != nil && !errors.Is(err, io.EOF) {
		return nil
	}

	return head[:n]
}

// followRotation re-checks via whichever route resolved the source (route-agnostic) and switches within the sample.
// pg_rotate_logfile() leaves the old file neither truncated nor removed, so drain-then-switch is exact.
// One rotation per interval is lossless; a second skips the middle file for routes 1/2 (name only the current file) — glob doesn't have this gap.
func (t *logTail) followRotation(ctx context.Context, q Querier, read *tailRead) {
	path, format, ok := recheckLogSource(ctx, q, t.settings, t.source)
	if !ok || path == t.source.path {
		return
	}

	superseded := t.source

	// Flush before closing: previous_to_offset must reflect where the superseded
	// file was actually left, including any held event.
	t.flushPending(read)
	supersededTo := t.consumed()

	t.closeFile()

	rotated := t.source
	rotated.path = path
	rotated.format = format

	// Always openAtStart here: EOF-seek is only for t0, not every file the window sees.
	if err := t.open(rotated, openAtStart); err != nil {
		// Not adopted: this block stays a measurement of the drained file; the next sample re-resolves and reports reason=unreadable.
		// Position remembered so a target that becomes readable later resumes here instead of opening at its own EOF, which would skip everything written meanwhile under a fake matched=0.
		t.lostPath, t.lostOffset = superseded.path, t.offset
		t.source = superseded

		return
	}

	t.source = rotated

	read.rotated = true
	read.previousPath = superseded.path
	read.previousTo = supersededTo
	read.from = 0

	t.readOpenFile(read, time.Time{})
}

// recheckLogSource asks one route - the one that answered - where the current
// file is now.
func recheckLogSource(ctx context.Context, q Querier, s logSettings, source logSource) (string, logFormat, bool) {
	switch source.resolvedBy {
	case resolvedByCurrentLogfiles:
		raw, format, _, ok := resolveFromCurrentLogfiles(s)
		if !ok {
			return "", "", false
		}

		path, ok := resolveLogfile(raw, s.dataDirectory)

		return path, format, ok

	case resolvedByFunction:
		raw, format, _, _, ok := resolveFromFunction(ctx, q, s)
		if !ok {
			return "", "", false
		}

		path, ok := resolveLogfile(raw, s.dataDirectory)

		return path, format, ok

	case resolvedByGlob:
		raw, format, _, ok := resolveFromGlob(s)
		if !ok {
			return "", "", false
		}

		return raw, format, true
	}

	return "", "", false
}

// logReadChunk bounds a single Read so LogDrainBudget can be checked between reads.
const logReadChunk = 1 << 20

// readOpenFile reads to the size the handle had when this sample looked, matching as it goes.
// deadline is zero for a scheduled sample (bounded by the window), set for the drain (bounded by nothing else).
func (t *logTail) readOpenFile(read *tailRead, deadline time.Time) {
	if t.file == nil {
		return
	}

	info, err := t.file.Stat()
	if err != nil {
		return
	}

	size := info.Size()

	t.checkTruncation(read, size)

	if size <= t.offset {
		return
	}

	if size-t.offset > MaxScanBytes {
		// Seeks to present: skipped_bytes= signals a possible missed event, distinct
		// from matched=0.
		t.flushPending(read)

		read.scanTruncated = true
		read.skipped += size - t.offset
		t.offset = size

		return
	}

	for t.offset < size {
		if !deadline.IsZero() && time.Now().After(deadline) {
			return
		}

		chunk := make([]byte, min(size-t.offset, logReadChunk))

		n, err := t.file.ReadAt(chunk, t.offset)
		if n > 0 {
			t.offset += int64(n)
			t.consume(read, chunk[:n])
		}

		if err != nil {
			return
		}
	}
}

// checkTruncation catches log_truncate_on_rotation rewriting the file in place.
// Size shrinking catches the ordinary case; the head fingerprint catches truncate-then-regrow past the saved offset, which a size check alone reads as normal growth.
func (t *logTail) checkTruncation(read *tailRead, size int64) {
	head := t.readHead()

	truncated := size < t.offset || (len(t.head) > 0 && !bytes.HasPrefix(head, t.head))
	if !truncated {
		if len(head) > len(t.head) {
			t.head = head
		}

		return
	}

	t.flushPending(read)

	// Same rebase as rotation: from_offset would otherwise point at bytes that no
	// longer exist, making to_offset - from_offset negative.
	read.previousTo = t.offset
	read.from = 0

	t.offset = 0
	t.head = head
	read.truncated = true
}

// flushPending ends the pending run at a gap/close. A held event is written with partial_event=true ("end unproven", not "bytes missing").
// A held incomplete unit is dropped: appending post-gap bytes would weld two unrelated pieces into one fake event.
func (t *logTail) flushPending(read *tailRead) {
	if len(t.pending) == 0 {
		t.pendingIsEvent = false
		return
	}

	if t.pendingIsEvent {
		read.events = appendEvent(read.events, t.pending, read)
		read.matched++
		read.partial = true
	} else {
		read.carryDropped = true
	}

	t.pending = nil
	t.pendingIsEvent = false
}

// consume runs the matcher over the pending bytes plus the new ones.
func (t *logTail) consume(read *tailRead, chunk []byte) {
	if t.full {
		// At cap: offsets still advance so the file stays honest about what was
		// skipped; the pending run ends here like any other gap.
		if len(t.pending) > 0 {
			read.carryDropped = true
		}

		t.pending = nil
		t.pendingIsEvent = false

		return
	}

	data := chunk
	if len(t.pending) > 0 {
		data = append(bytes.Clone(t.pending), chunk...)
	}

	events, pending, pendingIsEvent, matched := matchEvents(data, t.source.format, t.match, read)

	read.events = append(read.events, events...)
	read.matched += matched

	t.pending = pending
	t.pendingIsEvent = pendingIsEvent

	// Caps how long an incomplete event can hold the window open.
	if len(t.pending) > MaxEventBytes {
		t.flushPending(read)
	}
}

// matchEvents dispatches to the format-specific matcher.
func matchEvents(data []byte, format logFormat, m eventMatch, read *tailRead) (
	events [][]byte, pending []byte, pendingIsEvent bool, matched int,
) {
	switch format {
	case logFormatCSV:
		return matchCSVLog(data, m, read)

	case logFormatJSON:
		return matchJSONLog(data, m, read)
	}

	return matchStderr(data, m, read)
}

// matchStderr's boundary rule: an event is the primary line plus every following TAB-continued or secondary-field line, stopping at the first that's neither.
// Continuation lines are TAB-prefixed regardless of log_line_prefix (measured); the report continues past DETAIL into HINT/CONTEXT/STATEMENT, the two lines a DBA reads first.
// Ambiguous lines are treated as part of the current event: over-copying loses nothing, under-copying loses evidence permanently.
func matchStderr(data []byte, m eventMatch, read *tailRead) (
	events [][]byte, pending []byte, pendingIsEvent bool, matched int,
) {
	var (
		inEvent    bool
		eventStart int
		eventLines int
		prefix     string
	)

	pos := 0

	for pos < len(data) {
		next := bytes.IndexByte(data[pos:], '\n')
		if next < 0 {
			break
		}

		lineStart, lineEnd := pos, pos+next+1
		line := string(data[lineStart : lineEnd-1])
		pos = lineEnd

		if inEvent {
			if !isNewStderrEntry(line, prefix) {
				eventLines++

				if eventLines >= MaxEventLines || lineEnd-eventStart >= MaxEventBytes {
					events = appendEvent(events, data[eventStart:lineEnd], read)
					matched++
					inEvent = false
				}

				continue
			}

			events = appendEvent(events, data[eventStart:lineStart], read)
			matched++
			inEvent = false
		}

		if found, ok := m.matchStderrLine(line); ok {
			inEvent, eventStart, eventLines, prefix = true, lineStart, 1, found
		}
	}

	if inEvent {
		return events, bytes.Clone(data[eventStart:]), true, matched
	}

	return events, bytes.Clone(data[pos:]), false, matched
}

// matchStderrLine derives the report's prefix by finding the severity keyword.
// Every line of a report shares the identical expanded prefix, so log_line_prefix itself never needs parsing.
func (m eventMatch) matchStderrLine(line string) (prefix string, ok bool) {
	at, message := stderrSeverity(line)

	if at < 0 || !m.matchesMessage(message) {
		return "", false
	}

	return line[:at], true
}

// stderrSeverity finds the earliest severity keyword in a line and returns where it
// starts - everything before it is the expanded log_line_prefix - and the message
// after it. -1 when the line opens no entry.
func stderrSeverity(line string) (at int, message string) {
	at = -1

	for _, keyword := range severityKeywords {
		i := strings.Index(line, keyword+":")
		if i < 0 || (at >= 0 && i >= at) {
			continue
		}

		rest, found := keywordAt(line[i:], severityKeywords)
		if !found {
			continue
		}

		at, message = i, rest
	}

	return at, message
}

// isNewStderrEntry: a line without the report's prefix is a different entry by construction (the prefix holds a timestamp), judged by scanning the whole line.
// Secondary keyword checked first so text like "...ERROR:" inside a DETAIL line doesn't falsely end the event.
func isNewStderrEntry(line, prefix string) bool {
	if strings.HasPrefix(line, "\t") {
		return false
	}

	if strings.HasPrefix(line, prefix) {
		rest := line[len(prefix):]

		if _, ok := keywordAt(rest, secondaryKeywords); ok {
			return false
		}

		_, ok := keywordAt(rest, severityKeywords)

		return ok
	}

	severity := keywordIndex(line, severityKeywords)
	secondary := keywordIndex(line, secondaryKeywords)

	if secondary >= 0 && (severity < 0 || secondary < severity) {
		return false
	}

	return severity >= 0
}

// csvlog column indices, stable across PG 14-18 (26 columns; the two added in 14 are the last two).
// FieldsPerRecord = -1 tolerates a version that appends more; a short record is treated as a corrupt tail, not a panic.
const (
	csvStateIndex   = 12
	csvMessageIndex = 13
	csvDetailIndex  = 14

	// csvQueryIDIndex is the 26th column, added in 14 with leader_pid before it.
	csvQueryIDIndex = 25
)

// matchCSVLog reads records, not lines (a deadlock's quoted DETAIL field spans several physical lines with real newlines).
// Matches are copied as original bytes via InputOffset, never re-encoded — encoding/csv's quoting rules aren't the server's.
func matchCSVLog(data []byte, m eventMatch, read *tailRead) (
	events [][]byte, pending []byte, pendingIsEvent bool, matched int,
) {
	complete, tail := splitAtLastLine(data)

	reader := csv.NewReader(bytes.NewReader(complete))
	reader.FieldsPerRecord = -1
	reader.ReuseRecord = true

	for {
		start := reader.InputOffset()

		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return events, bytes.Clone(tail), false, matched
		}

		if err != nil {
			// ErrQuote here means a quoted field (e.g. DETAIL) was cut mid-newline at
			// a flush boundary — held from the record's start regardless of match.
			return events, bytes.Clone(data[start:]), false, matched
		}

		end := reader.InputOffset()

		if len(record) > csvMessageIndex && m.matches(record[csvStateIndex], record[csvMessageIndex]) {
			events = appendEvent(events, complete[start:end], read)
			matched++
		}
	}
}

// jsonEntry: jsonlog is one line per event, the only format with no boundary problem.
type jsonEntry struct {
	// StateCode is absent from the line when the code is 00000 - every LOG entry -
	// where csvlog writes the column; matchJSONLog supplies it.
	StateCode string `json:"state_code"`
	Message   string `json:"message"`
	Detail    string `json:"detail"`

	// QueryID is a JSON number in the file; zero is what the server writes when none
	// was computed.
	QueryID int64 `json:"query_id"`
}

func matchJSONLog(data []byte, m eventMatch, read *tailRead) (
	events [][]byte, pending []byte, pendingIsEvent bool, matched int,
) {
	pos := 0

	for pos < len(data) {
		next := bytes.IndexByte(data[pos:], '\n')
		if next < 0 {
			break
		}

		lineStart, lineEnd := pos, pos+next+1
		pos = lineEnd

		var entry jsonEntry
		if err := json.Unmarshal(data[lineStart:lineEnd-1], &entry); err != nil {
			continue
		}

		// The writer omits state_code when the code is successful completion
		// (measured on 15 through 18: no LOG line carries the key), so an absent code
		// is 00000 here as it is in csvlog's column. Without this a matcher that pairs
		// 00000 with its message - the explain and checkpoint tails - never fires on
		// jsonlog.
		if entry.StateCode == "" {
			entry.StateCode = "00000"
		}

		if m.matches(entry.StateCode, entry.Message) {
			events = appendEvent(events, data[lineStart:lineEnd], read)
			matched++
		}
	}

	return events, bytes.Clone(data[pos:]), false, matched
}

// splitAtLastLine keeps a record-oriented matcher from parsing bytes the server
// has not finished writing.
func splitAtLastLine(data []byte) (complete, tail []byte) {
	at := bytes.LastIndexByte(data, '\n')
	if at < 0 {
		return nil, data
	}

	return data[:at+1], data[at+1:]
}

// appendEvent truncates in bytes, never runes: SQL_ASCII log bytes must pass through unencoded.
// Trailing newline is for readability/grep only — bytes= is the actual parsing contract.
// The event is copied, never aliased: readEvents' caller keeps it past the read.
func appendEvent(events [][]byte, event []byte, read *tailRead) [][]byte {
	if len(event) > MaxEventBytes {
		read.eventsTruncated++

		truncated := make([]byte, 0, MaxEventBytes)
		truncated = append(truncated, event[:MaxEventBytes-len(eventTruncationMark)]...)
		truncated = append(truncated, eventTruncationMark...)

		event = truncated
	}

	owned := make([]byte, 0, len(event)+1)
	owned = append(owned, event...)

	if len(owned) > 0 && owned[len(owned)-1] != '\n' {
		owned = append(owned, '\n')
	}

	return append(events, owned)
}
