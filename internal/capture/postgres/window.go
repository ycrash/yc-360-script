package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"
)

// One connection per timeline: pgx connections aren't safe for concurrent use.

// The connections a run opens, one per speed, so a slow read on one never delays
// the samples on another. Artifacts name theirs; those naming the same one share
// its timeline.
const (
	ConnectionFast      = "fast"
	ConnectionNormal    = "normal"
	ConnectionExpensive = "expensive"
)

// connectionOrder is the dialling order: fast first, so on a server with one slot
// left, session state is what gets it. A name not listed is dialled after these.
var connectionOrder = []string{ConnectionFast, ConnectionNormal, ConnectionExpensive}

// Status values for an artifact's closing block.
const (
	// StatusComplete: an empty database still yields status=complete.
	StatusComplete = "complete"

	// StatusPartial: window ran its course but a sample failed.
	StatusPartial = "partial"

	StatusCancelled        = "cancelled"
	StatusDeadlineExceeded = "deadline_exceeded"

	// StatusConnectionLost: the driver closed the connection mid-window. That
	// connection's timeline stopped at the sample that found out, its artifacts keep
	// what they had written, and the other connections' timelines go on. No reconnect
	// is attempted: a new connection would restart every delta baseline under the
	// same artifact. A window stopped by its own context reports the stop instead:
	// the driver closes the connection then too, but the stop is the cause.
	StatusConnectionLost = "connection_lost"

	// StatusConnectFailed: the file still exists, the only record the run tried.
	StatusConnectFailed = "connect_failed"
)

// Schedule: Every's offsets stay strictly inside the window, so the closing tick is never an Every collector's.
// Periodic's last offset is the close itself, so it counts towards moduleDeadline's closing tick.
type Schedule struct {
	kind     scheduleKind
	interval time.Duration
}

type scheduleKind int

const (
	scheduleStartEnd scheduleKind = iota
	scheduleEvery
	scheduleOnce
	schedulePeriodic
)

func StartEnd() Schedule {
	return Schedule{kind: scheduleStartEnd}
}

func Once() Schedule {
	return Schedule{kind: scheduleOnce}
}

// Every's last sample lands one interval before the window closes, not at it.
func Every(d time.Duration) Schedule {
	return Schedule{kind: scheduleEvery, interval: d}
}

// Periodic is Every plus a sample at the close. A window shorter than the interval
// still gives two samples, so the first reading always has a second to be read
// against. Periodic with no interval is StartEnd.
func Periodic(d time.Duration) Schedule {
	return Schedule{kind: schedulePeriodic, interval: d}
}

// offsets are computed before the window opens, so the preamble can state samples_expected.
func (s Schedule) offsets(window time.Duration) []time.Duration {
	if window <= 0 {
		return []time.Duration{0}
	}

	// Must run before the "not Every" check, or Once samples twice.
	if s.kind == scheduleOnce {
		return []time.Duration{0}
	}

	// Before both Every checks below. Every with no interval gives one sample;
	// Periodic with no interval must still give the two.
	if s.kind == schedulePeriodic {
		return periodicOffsets(s.interval, window)
	}

	if s.kind != scheduleEvery {
		return []time.Duration{0, window}
	}

	if s.interval <= 0 {
		return []time.Duration{0}
	}

	offsets := make([]time.Duration, 0, int(window/s.interval)+1)
	for at := time.Duration(0); at < window; at += s.interval {
		offsets = append(offsets, at)
	}

	return offsets
}

// periodicOffsets steps through the window like Every, then adds the close.
func periodicOffsets(interval, window time.Duration) []time.Duration {
	if interval <= 0 {
		return []time.Duration{0, window}
	}

	offsets := make([]time.Duration, 0, int(window/interval)+2)
	for at := time.Duration(0); at < window; at += interval {
		offsets = append(offsets, at)
	}

	// Drop the last step if it lands within half an interval of the close. Periodic(119s)
	// on a 120s window would sample at 119s and 120s, and a one-second gap is noise the
	// counters cannot be read against. Never drop the opening sample.
	if last := len(offsets) - 1; last > 0 && window-offsets[last] < interval/2 {
		offsets = offsets[:last]
	}

	return append(offsets, window)
}

func (s Schedule) name() string {
	switch s.kind {
	case scheduleEvery:
		return "every"

	case scheduleOnce:
		return "once"

	case schedulePeriodic:
		return "periodic"
	}

	return "start_end"
}

func (s Schedule) intervalText() string {
	if (s.kind != scheduleEvery && s.kind != schedulePeriodic) || s.interval <= 0 {
		return ""
	}

	return windowSeconds(s.interval)
}

// Artifact is what the window needs to write a preamble/closing block, even if the collector never runs.
type Artifact struct {
	// Name is the source= for window-written blocks; collector blocks name themselves.
	Name string

	FileName string

	// Scope is "database" or "cluster" - what the block's rows are about.
	Scope string

	Schedule Schedule

	// SampleBudget: assumed cost of one sample, summed across the collectors sharing a connection's closing tick. Zero means DefaultSampleBudget.
	SampleBudget time.Duration

	// Format is the body format, formatCSV when empty.
	// Header-only blocks (preamble/closing/stub) still carry the real format=, or a receiver dispatching on the first block misparses the file.
	Format string

	// Connection is the connection the artifact samples on (ConnectionFast and the
	// two others). Empty names one of its own like any other name, which a test's
	// window of fakes shares.
	Connection string

	// Version is the file's format version, v= on each of its blocks. Zero is
	// defaultArtifactVersion.
	Version int
}

func artifactFormat(artifact Artifact) string {
	if artifact.Format == "" {
		return formatCSV
	}

	return artifact.Format
}

func artifactVersion(artifact Artifact) int {
	if artifact.Version == 0 {
		return defaultArtifactVersion
	}

	return artifact.Version
}

type Collector interface {
	Artifact() Artifact

	// Sample writes blocks or nothing+error; an error here means the write failed, not the read.
	// ctx is the window's; a collector running multiple statements bounds each itself.
	Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
}

// Opening: written after the preamble, before dial, so it lands on every failure path.
type Opening interface {
	WriteOpening(w io.Writer, s SampleContext) error
}

// Closing: called with no context; must bound its own work.
// Exists because Every's offsets stop short of the window close.
type Closing interface {
	WriteClosing(w io.Writer, s SampleContext) error
}

type SampleContext struct {
	// At is one clock read shared by every block in a sample; equal ts= is by construction.
	At time.Time

	Index int // 1-based: sample=1 with no sample=2 is partial

	// Total is SamplesExpected; Index == Total marks the window's close.
	Total int

	// Database/DBID: current_database() and its OID; pre-connect, Database is the configured name and DBID is empty.
	Database string
	DBID     string

	// HasPgStatCheckpointer: capability check (not version) for PostgreSQL 17's moved columns; false when identify fails.
	HasPgStatCheckpointer bool

	// ConnectDuration is the cost of dialling the collector's own connection. The
	// window owns the connections, so this is where a collector learns it. Zero on
	// every path that never dialled.
	ConnectDuration time.Duration

	// redact centralizes the window's password redaction.
	redact func(error) string
}

func (s SampleContext) errorText(err error) string {
	if s.redact != nil {
		return s.redact(err)
	}

	return errorText(err, "")
}

// ArtifactResult: every field but IOErr is also written into the artifact; IOErr is what it can't record about itself.
type ArtifactResult struct {
	Artifact        Artifact
	File            *os.File
	Status          string
	SamplesExpected int
	SamplesWritten  int

	// Err is the last capture-level error, already redacted. It reaches the agent
	// log, never an artifact row, so it keeps the failure in full.
	Err string

	IOErr error
}

func (r ArtifactResult) writable() bool { return r.File != nil && r.IOErr == nil }

// windowConn is what the window needs of a connection; *Conn satisfies it, tests fake it.
type windowConn interface {
	RowQuerier
	Close(ctx context.Context) error
}

// connectTimer is the part of *Conn reporting the dial's cost. Asserted rather
// than added to windowConn, so a fake reports nothing - the truth about a dial it
// never made.
type connectTimer interface {
	ConnectDuration() time.Duration
}

// connectionState is the part of *Conn that knows the driver closed the connection.
// Asserted the same way: a fake that cannot say is a live connection.
type connectionState interface {
	Lost() bool
}

func connectionLost(conn windowConn) bool {
	state, ok := conn.(connectionState)

	return ok && state.Lost()
}

// lossReporter is the part of *Conn that kept the error the driver closed on.
type lossReporter interface {
	LossError() error
}

// connectionLossText is that error, redacted; empty for a connection that kept none.
func connectionLossText(conn windowConn, password string) string {
	reporter, ok := conn.(lossReporter)
	if !ok {
		return ""
	}

	return errorText(reporter.LossError(), password)
}

func connectDuration(conn windowConn) time.Duration {
	if timer, ok := conn.(connectTimer); ok {
		return timer.ConnectDuration()
	}

	return 0
}

// Window owns a run's connections, one for each name its collectors' artifacts
// give, and one clock: every connection's timeline counts its offsets from the
// same start, and they run at the same time.
type Window struct {
	Target     Target
	Duration   time.Duration
	Collectors []Collector

	// Test seams, zero in production: now/after skip real waits, connect skips a server, grace shortens the deadline.
	now     func() time.Time
	after   func(time.Duration) <-chan time.Time
	connect func(ctx context.Context, t Target) (windowConn, error)
	grace   time.Duration
}

// moduleDeadline: Duration + the closing tick's SampleBudgets + WindowCloseMargin.
// A connection's closing tick runs its collectors one after another, so their
// budgets are summed; the connections run at the same time, so the deadline covers
// the longest of those sums.
func (w *Window) moduleDeadline() time.Duration {
	if w.grace > 0 {
		return w.Duration + w.grace
	}

	budgets := map[string]time.Duration{}
	budget := time.Duration(0)

	for _, collector := range w.Collectors {
		artifact := collector.Artifact()

		offsets := artifact.Schedule.offsets(w.Duration)
		if offsets[len(offsets)-1] == w.Duration {
			budgets[artifact.Connection] += sampleBudget(artifact)
			budget = max(budget, budgets[artifact.Connection])
		}
	}

	// No collectors on the closing tick: still reserve a default budget.
	if budget == 0 {
		budget = DefaultSampleBudget
	}

	return w.Duration + budget + WindowCloseMargin
}

func sampleBudget(artifact Artifact) time.Duration {
	if artifact.SampleBudget > 0 {
		return artifact.SampleBudget
	}

	return DefaultSampleBudget
}

// connectionTimeline is one connection, the collectors that sample on it, and how
// its part of the run ended.
type connectionTimeline struct {
	name string

	// collectors indexes Window.Collectors, in registration order.
	collectors []int

	conn      windowConn
	sampleCtx SampleContext

	// connectErr is the refused dial's artifact row value, connectDetail the same
	// failure in full, for the log. Both empty once connected.
	connectErr    string
	connectDetail string

	// stopped is the status that ended the timeline early, empty if it ran its
	// course; lostErr is the sample error that revealed a lost connection.
	stopped string
	lostErr string
}

// timelines groups the collectors by the connection their artifacts name, in
// dialling order, and maps each collector to its timeline.
func (w *Window) timelines() ([]*connectionTimeline, []*connectionTimeline) {
	var lines []*connectionTimeline

	owner := make([]*connectionTimeline, len(w.Collectors))
	byName := map[string]*connectionTimeline{}

	for i, collector := range w.Collectors {
		name := collector.Artifact().Connection

		line, ok := byName[name]
		if !ok {
			line = &connectionTimeline{name: name}
			byName[name] = line
			lines = append(lines, line)
		}

		line.collectors = append(line.collectors, i)
		owner[i] = line
	}

	rank := func(name string) int {
		if i := slices.Index(connectionOrder, name); i >= 0 {
			return i
		}

		return len(connectionOrder)
	}

	slices.SortStableFunc(lines, func(a, b *connectionTimeline) int {
		return cmp.Compare(rank(a.name), rank(b.name))
	})

	return lines, owner
}

// Run returns one result per artifact; no error return, since a refused connection is itself a captured outcome.
// Files are left open at their end offset for the caller to upload and close.
func (w *Window) Run(ctx context.Context) []ArtifactResult {
	results := make([]ArtifactResult, len(w.Collectors))
	for i, collector := range w.Collectors {
		artifact := collector.Artifact()

		results[i] = ArtifactResult{
			Artifact:        artifact,
			SamplesExpected: len(artifact.Schedule.offsets(w.Duration)),
		}
	}

	w.openArtifacts(results, w.baseSampleContext())

	lines, owner := w.timelines()

	w.dialAll(ctx, lines)

	for _, line := range lines {
		if line.conn != nil {
			defer w.disconnect(line.conn)
		}
	}

	if slices.ContainsFunc(lines, func(line *connectionTimeline) bool { return line.conn != nil }) {
		w.runTimelines(ctx, lines, owner, results)
	}

	w.closeArtifacts(results, owner)

	return results
}

// runTimelines samples every connected timeline at once and returns when all have ended.
func (w *Window) runTimelines(
	ctx context.Context, lines, owner []*connectionTimeline, results []ArtifactResult,
) {
	// Armed after dial, so connect/identify time doesn't eat the grace the final sample needs.
	ctx, cancel := context.WithTimeout(ctx, w.moduleDeadline())
	defer cancel()

	// One start for every timeline, so equal offsets mean the same moment on each.
	start := w.clock()

	var wg sync.WaitGroup

	for _, line := range lines {
		if line.conn == nil {
			continue
		}

		wg.Add(1)

		go func() {
			defer wg.Done()

			line.stopped, line.lostErr = w.sample(ctx, line, start, owner, results)
		}()
	}

	wg.Wait()
}

// dialAll opens each timeline's connection in turn. A refusal for want of a slot
// fails only that timeline, and the next is still dialled: a slot may have come
// free, and the timelines that connect still run. Any other failure is the
// target's - its address, TLS, credentials or database - and would fail every
// dial the same way, so the timelines not yet dialled take it without trying.
func (w *Window) dialAll(ctx context.Context, lines []*connectionTimeline) {
	var targetErr error

	for _, line := range lines {
		line.sampleCtx = w.baseSampleContext()

		err := targetErr
		if err == nil {
			line.conn, err = w.dial(ctx)
		}

		if err != nil {
			// Two renderings of one failure: the token for the artifact row, which a
			// reader matches on, and the full text for the log. A refusal at
			// max_connections, a role's CONNECTION LIMIT and a database's are all
			// SQLSTATE 53300 with three different fixes, told apart only by the text.
			line.conn = nil
			line.connectErr = ConnectErrorText(err, w.Target)
			line.connectDetail = errorText(err, w.Target.Password)

			if !errors.Is(err, ErrTooManyConnections) {
				targetErr = err
			}

			continue
		}

		line.sampleCtx = w.identify(ctx, line.conn)
		line.sampleCtx.ConnectDuration = connectDuration(line.conn)
	}
}

// openArtifacts writes each preamble, including samples_expected - unrecoverable from a truncated file.
func (w *Window) openArtifacts(results []ArtifactResult, sampleCtx SampleContext) {
	for i := range results {
		artifact := results[i].Artifact

		file, err := os.Create(artifact.FileName)
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to create %s: %w", artifact.FileName, err)
			continue
		}
		results[i].File = file

		err = writeVersionedBlockHeader(file, artifact.Name, artifactVersion(artifact), artifact.Scope,
			artifactFormat(artifact), []headerField{
				{"db", sampleCtx.Database},
				{"dbid", sampleCtx.DBID},
				{"status", "started"},
				{"window", windowSeconds(w.Duration)},
				// interval= is empty without a cadence; samples_expected needs it to make sense.
				{"schedule", artifact.Schedule.name()},
				{"interval", artifact.Schedule.intervalText()},
				{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
			}, w.clock())
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		syncArtifact(file)

		w.writeOpening(&results[i], w.Collectors[i], sampleCtx)
	}
}

func (w *Window) writeOpening(result *ArtifactResult, collector Collector, sampleCtx SampleContext) {
	opening, ok := collector.(Opening)
	if !ok {
		return
	}

	// sampleCtx.At is zero here; set it to the preamble's clock read.
	at := sampleCtx
	at.At = w.clock()

	if err := opening.WriteOpening(result.File, at); err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", result.Artifact.FileName, err)
		return
	}

	syncArtifact(result.File)
}

// writeClosing: a collector's last write, guarded by writable(); no context, since closeArtifacts has none.
func (w *Window) writeClosing(result *ArtifactResult, collector Collector, sampleCtx SampleContext) {
	closing, ok := collector.(Closing)
	if !ok || !result.writable() {
		return
	}

	at := sampleCtx
	at.At = w.clock()

	if err := closing.WriteClosing(result.File, at); err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", result.Artifact.FileName, err)
		return
	}

	syncArtifact(result.File)
}

// closeArtifacts is the last pass, run with no context so it can record an expired deadline.
// Each artifact closes with its own connection's outcome: the status that ended that
// timeline early, if any; for a lost connection, the sample error that revealed it,
// already redacted, as connection_error= so each file says on its own what ended it;
// and for a connection never made, connect_error=, the artifact row's value, while
// the same failure in full goes to the caller's log. Only the row is a contract.
func (w *Window) closeArtifacts(results []ArtifactResult, owner []*connectionTimeline) {
	// Drains run before the clock read below, so the closing timestamp doesn't predate their bytes.
	for i := range results {
		line := owner[i]

		results[i].Status = artifactStatus(results[i], line.stopped, line.connectErr)

		if line.connectErr != "" {
			results[i].Err = line.connectDetail
		}

		if line.stopped == StatusConnectionLost && results[i].Err == "" {
			results[i].Err = line.lostErr
		}

		// Must run after Status is set: a closing-pass IOErr makes writable() false, skipping the closing block.
		w.writeClosing(&results[i], w.Collectors[i], line.sampleCtx)
	}

	at := w.clock()

	for i := range results {
		if !results[i].writable() {
			continue
		}

		line := owner[i]

		fields := []headerField{
			{"db", line.sampleCtx.Database},
			{"dbid", line.sampleCtx.DBID},
			{"status", results[i].Status},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
			{"samples_written", strconv.Itoa(results[i].SamplesWritten)},
		}

		if line.connectErr != "" {
			fields = append(fields, headerField{"connect_error", line.connectErr})
		}

		if line.stopped == StatusConnectionLost {
			fields = append(fields, headerField{"connection_error", line.lostErr})
		}

		artifact := results[i].Artifact
		if err := writeVersionedBlockHeader(results[i].File, artifact.Name, artifactVersion(artifact), artifact.Scope,
			artifactFormat(artifact), fields, at); err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		syncArtifact(results[i].File)
	}
}

// A stopped window overrides the sample counts: "cancelled after one of two" isn't "one of two failed".
func artifactStatus(result ArtifactResult, stopped, connectErr string) string {
	switch {
	case connectErr != "":
		return StatusConnectFailed
	case stopped != "":
		return stopped
	case result.SamplesWritten == result.SamplesExpected:
		return StatusComplete
	default:
		return StatusPartial
	}
}

type sampleEvent struct {
	at time.Duration

	// collector indexes Window.Collectors; also the tie-break for equal ticks.
	collector int

	// index is that collector's own 1-based sample number; unrelated across collectors.
	index int
}

// timeline merges every collector's offsets into one ordered walk, so cadences share a connection.
func timeline(collectors []Collector, window time.Duration) []sampleEvent {
	var events []sampleEvent

	for i, collector := range collectors {
		for n, at := range collector.Artifact().Schedule.offsets(window) {
			events = append(events, sampleEvent{at: at, collector: i, index: n + 1})
		}
	}

	slices.SortStableFunc(events, func(a, b sampleEvent) int {
		if order := cmp.Compare(a.at, b.at); order != 0 {
			return order
		}

		// Tie-break is registration order, so a shared tick runs cheap reads before expensive ones.
		return cmp.Compare(a.collector, b.collector)
	})

	return events
}

// sample walks one connection's timeline serially; returns the status that stopped it
// early, or empty if complete, and for a lost connection the sample error that revealed it.
func (w *Window) sample(
	ctx context.Context, line *connectionTimeline, start time.Time, owner []*connectionTimeline,
	results []ArtifactResult,
) (stopped, lostErr string) {
	for _, event := range timeline(w.Collectors, w.Duration) {
		if owner[event.collector] != line {
			continue
		}

		// Offsets are absolute: a slow sample doesn't delay the next tick; an overdue tick fires immediately.
		if wait := start.Add(event.at).Sub(w.clock()); wait > 0 {
			select {
			case <-ctx.Done():
				return stoppedStatus(ctx), ""
			case <-w.timer(wait):
			}
		}

		if ctx.Err() != nil {
			return stoppedStatus(ctx), ""
		}

		// A failed sample on a live connection is that sample's stub block and the next
		// tick proceeds; on a connection the driver has closed, whether the sample
		// returned the error or folded it into its own block, every later tick would
		// fail the same way, so the timeline stops here and its artifacts say why.
		if lost, err := w.sampleOnce(ctx, line.conn, line.sampleCtx, results, event); lost {
			// The driver also closes the connection when the window's context ends
			// mid-statement. That is a cancel or a deadline, and it says so.
			if ctx.Err() != nil {
				return stoppedStatus(ctx), ""
			}

			return StatusConnectionLost, err
		}
	}

	return "", ""
}

// sampleOnce runs one tick; lost reports a connection the driver has closed, with the
// redacted error it closed on - or, for a connection that kept none, the sample's.
func (w *Window) sampleOnce(
	ctx context.Context, conn windowConn, sampleCtx SampleContext, results []ArtifactResult, event sampleEvent,
) (lost bool, err string) {
	result := &results[event.collector]
	if !result.writable() {
		return false, ""
	}

	at := sampleCtx
	at.Index = event.index
	at.Total = result.SamplesExpected
	at.At = w.clock()

	if sampleErr := w.Collectors[event.collector].Sample(ctx, conn, result.File, at); sampleErr != nil {
		w.writeSampleError(result, at, sampleErr)
	} else {
		result.SamplesWritten++
	}

	// Checked whatever the sample returned: a collector that localises failures to a
	// block carries the loss on that block's header and reports nothing.
	if !connectionLost(conn) {
		return false, ""
	}

	if lossErr := connectionLossText(conn, w.Target.Password); lossErr != "" {
		result.Err = lossErr
	}

	return true, result.Err
}

// writeSampleError records a failed sample so numbering doesn't gap silently; the block names the artifact, not a view.
func (w *Window) writeSampleError(result *ArtifactResult, sampleCtx SampleContext, sampleErr error) {
	result.Err = errorText(sampleErr, w.Target.Password)

	artifact := result.Artifact

	err := writeVersionedBlockHeader(result.File, artifact.Name, artifactVersion(artifact), artifact.Scope,
		artifactFormat(artifact), []headerField{
			{"db", sampleCtx.Database},
			{"dbid", sampleCtx.DBID},
			{"sample", strconv.Itoa(sampleCtx.Index)},
			{"sample_error", result.Err},
		}, sampleCtx.At)
	if err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
	}
}

// OID comes from pg_database, not a name cast: survives mid-run renames.
// Capability expressions must match serverFactsSQL's exactly.
const currentDatabaseSQL = `SELECT current_database()::text,
       (SELECT oid::text FROM pg_catalog.pg_database WHERE datname = current_database()),
       to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL`

// identify reads the database, OID and capability flags once for every collector.
// On failure, HasPgStatCheckpointer stays false, so a PG17 server gets the pre-17 statement and errors.
func (w *Window) identify(ctx context.Context, conn RowQuerier) SampleContext {
	sampleCtx := w.baseSampleContext()

	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	var database string
	var dbid *string
	var hasPgStatCheckpointer bool

	if err := conn.QueryRow(stmtCtx, currentDatabaseSQL).
		Scan(&database, &dbid, &hasPgStatCheckpointer); err != nil {
		return sampleCtx
	}

	sampleCtx.Database = database
	if dbid != nil {
		sampleCtx.DBID = *dbid
	}
	sampleCtx.HasPgStatCheckpointer = hasPgStatCheckpointer

	return sampleCtx
}

func (w *Window) dial(ctx context.Context) (windowConn, error) {
	connectCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	if w.connect != nil {
		return w.connect(connectCtx, w.Target)
	}

	conn, err := Connect(connectCtx, w.Target)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

// disconnect uses a fresh context: the window's is usually expired, and closing on it would leak the session.
func (w *Window) disconnect(conn windowConn) {
	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()

	_ = conn.Close(ctx)
}

func (w *Window) baseSampleContext() SampleContext {
	password := w.Target.Password

	return SampleContext{
		Database: w.Target.Database,
		redact:   func(err error) string { return errorText(err, password) },
	}
}

// context.WithTimeout reports parent cancellation as Canceled, its own timer as DeadlineExceeded.
func stoppedStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusDeadlineExceeded
	}

	return StatusCancelled
}

// windowSeconds renders as seconds, not time.Duration's form: 120s parses everywhere, 2m0s doesn't.
func windowSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// syncArtifact ignores Sync errors: bytes are already written; sync just insures against a crash before the next block.
func syncArtifact(file *os.File) {
	_ = file.Sync()
}

func (w *Window) clock() time.Time {
	if w.now != nil {
		return w.now()
	}

	return time.Now()
}

func (w *Window) timer(d time.Duration) <-chan time.Time {
	if w.after != nil {
		return w.after(d)
	}

	return time.After(d)
}
