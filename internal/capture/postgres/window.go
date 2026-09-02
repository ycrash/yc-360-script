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
	"time"
)

// One shared connection per run: pgx connections aren't safe for concurrent use.

// Status values for an artifact's closing block.
const (
	// StatusComplete: an empty database still yields status=complete.
	StatusComplete = "complete"

	// StatusPartial: window ran its course but a sample failed.
	StatusPartial = "partial"

	StatusCancelled        = "cancelled"
	StatusDeadlineExceeded = "deadline_exceeded"

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

// MaxDefaultInterval caps the derived cadence. Past it a longer window buys
// coarser samples and nothing else.
const MaxDefaultInterval = 5 * time.Minute

// DefaultInterval derives one run's cadence from its window, so a two-minute
// incident is not sampled at a two-hour capture's rate. Eight steps plus the
// close is nine samples for any window between the floor and the cap.
//
// The floor is StatementTimeout, the bound a sample's own statements carry, so a
// maxed-out sample can never outrun the tick behind it. A window shorter than the
// floor takes the floor anyway: Periodic reduces it to the bookend, which is the
// most such a window can honestly report.
func DefaultInterval(window time.Duration) time.Duration {
	if window <= 0 {
		return StatementTimeout
	}

	return min(max(window/8, StatementTimeout), MaxDefaultInterval)
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

	// SampleBudget: assumed cost of one sample, summed across collectors sharing the closing tick. Zero means DefaultSampleBudget.
	SampleBudget time.Duration

	// Format is the body format, formatCSV when empty.
	// Header-only blocks (preamble/closing/stub) still carry the real format=, or a receiver dispatching on the first block misparses the file.
	Format string
}

func artifactFormat(artifact Artifact) string {
	if artifact.Format == "" {
		return formatCSV
	}

	return artifact.Format
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

	// HasGenericPlan: EXPLAIN (GENERIC_PLAN) exists from PostgreSQL 16. A version test, not a
	// capability one - the option has no catalogue entry. False when identify fails, which
	// skips the generic-plan mode rather than attempting it.
	HasGenericPlan bool

	// ConnectDuration is the dial's cost. The window owns the connection, so this
	// is where a collector learns it. Zero on every path that never dialled.
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

func connectDuration(conn windowConn) time.Duration {
	if timer, ok := conn.(connectTimer); ok {
		return timer.ConnectDuration()
	}

	return 0
}

// Window owns one connection and one clock for every sampled artifact in a run.
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

// moduleDeadline: Duration + summed closing-tick SampleBudgets (tick can be shared, so summed not flat) + WindowCloseMargin.
func (w *Window) moduleDeadline() time.Duration {
	if w.grace > 0 {
		return w.Duration + w.grace
	}

	budget := time.Duration(0)

	for _, collector := range w.Collectors {
		artifact := collector.Artifact()

		offsets := artifact.Schedule.offsets(w.Duration)
		if offsets[len(offsets)-1] == w.Duration {
			budget += sampleBudget(artifact)
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

	sampleCtx := w.baseSampleContext()

	w.openArtifacts(results, sampleCtx)

	conn, err := w.dial(ctx)
	if err != nil {
		// Two renderings of one failure: the token for the artifact row, which a
		// reader matches on, and the full text for the log. A refusal at
		// max_connections, a role's CONNECTION LIMIT and a database's are all
		// SQLSTATE 53300 with three different fixes, told apart only by the text.
		w.closeArtifacts(results, sampleCtx, "",
			ConnectErrorText(err, w.Target), errorText(err, w.Target.Password))
		return results
	}
	defer w.disconnect(conn)

	sampleCtx = w.identify(ctx, conn)
	sampleCtx.ConnectDuration = connectDuration(conn)

	// Armed after dial, so connect/identify time doesn't eat the grace the final sample needs.
	ctx, cancel := context.WithTimeout(ctx, w.moduleDeadline())
	defer cancel()

	stopped := w.sample(ctx, conn, sampleCtx, results)

	w.closeArtifacts(results, sampleCtx, stopped, "", "")

	return results
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

		err = writeBlockHeaderFormat(file, artifact.Name, artifact.Scope, artifactFormat(artifact), []headerField{
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
// stopped: status that ended the window early (empty if it completed). connectErr: set only when there was never a connection.
// connectErr is the artifact row's value; connectDetail is the same failure in
// full, for the caller's log. Only the row is a contract.
func (w *Window) closeArtifacts(results []ArtifactResult, sampleCtx SampleContext, stopped, connectErr, connectDetail string) {
	// Drains run before the clock read below, so the closing timestamp doesn't predate their bytes.
	for i := range results {
		results[i].Status = artifactStatus(results[i], stopped, connectErr)

		if connectErr != "" {
			results[i].Err = connectDetail
		}

		// Must run after Status is set: a closing-pass IOErr makes writable() false, skipping the closing block.
		w.writeClosing(&results[i], w.Collectors[i], sampleCtx)
	}

	at := w.clock()

	for i := range results {
		if !results[i].writable() {
			continue
		}

		fields := []headerField{
			{"db", sampleCtx.Database},
			{"dbid", sampleCtx.DBID},
			{"status", results[i].Status},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
			{"samples_written", strconv.Itoa(results[i].SamplesWritten)},
		}

		if connectErr != "" {
			fields = append(fields, headerField{"connect_error", connectErr})
		}

		artifact := results[i].Artifact
		if err := writeBlockHeaderFormat(results[i].File, artifact.Name, artifact.Scope, artifactFormat(artifact), fields, at); err != nil {
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

// timeline merges every collector's offsets into one ordered walk, so cadences share one connection.
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

// sample walks the timeline serially; returns the status that stopped it early, or empty if complete.
func (w *Window) sample(ctx context.Context, conn RowQuerier, sampleCtx SampleContext, results []ArtifactResult) string {
	start := w.clock()

	for _, event := range timeline(w.Collectors, w.Duration) {
		// Offsets are absolute: a slow sample doesn't delay the next tick; an overdue tick fires immediately.
		if wait := start.Add(event.at).Sub(w.clock()); wait > 0 {
			select {
			case <-ctx.Done():
				return stoppedStatus(ctx)
			case <-w.timer(wait):
			}
		}

		if ctx.Err() != nil {
			return stoppedStatus(ctx)
		}

		w.sampleOnce(ctx, conn, sampleCtx, results, event)
	}

	return ""
}

func (w *Window) sampleOnce(ctx context.Context, conn RowQuerier, sampleCtx SampleContext, results []ArtifactResult, event sampleEvent) {
	result := &results[event.collector]
	if !result.writable() {
		return
	}

	at := sampleCtx
	at.Index = event.index
	at.Total = result.SamplesExpected
	at.At = w.clock()

	if err := w.Collectors[event.collector].Sample(ctx, conn, result.File, at); err != nil {
		w.writeSampleError(result, at, err)
		return
	}

	result.SamplesWritten++
}

// writeSampleError records a failed sample so numbering doesn't gap silently; the block names the artifact, not a view.
func (w *Window) writeSampleError(result *ArtifactResult, sampleCtx SampleContext, sampleErr error) {
	result.Err = errorText(sampleErr, w.Target.Password)

	artifact := result.Artifact

	err := writeBlockHeaderFormat(result.File, artifact.Name, artifact.Scope, artifactFormat(artifact), []headerField{
		{"db", sampleCtx.Database},
		{"dbid", sampleCtx.DBID},
		{"sample", strconv.Itoa(sampleCtx.Index)},
		{"sample_error", result.Err},
	}, sampleCtx.At)
	if err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
	}
}

// genericPlanSQL is shared with serverFactsSQL's has_generic_plan row, pinned equal by a
// test, so the flag collectors branch on and the fact the bundle reports cannot disagree.
const genericPlanSQL = `current_setting('server_version_num')::int >= 160000`

// OID comes from pg_database, not a name cast: survives mid-run renames.
// Capability expressions must match serverFactsSQL's exactly.
const currentDatabaseSQL = `SELECT current_database()::text,
       (SELECT oid::text FROM pg_catalog.pg_database WHERE datname = current_database()),
       to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL,
       ` + genericPlanSQL

// identify reads the database, OID and capability flags once for every collector.
// On failure, HasPgStatCheckpointer stays false, so a PG17 server gets the pre-17 statement and errors.
func (w *Window) identify(ctx context.Context, conn RowQuerier) SampleContext {
	sampleCtx := w.baseSampleContext()

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var database string
	var dbid *string
	var hasPgStatCheckpointer bool
	var hasGenericPlan bool

	if err := conn.QueryRow(stmtCtx, currentDatabaseSQL).
		Scan(&database, &dbid, &hasPgStatCheckpointer, &hasGenericPlan); err != nil {
		return sampleCtx
	}

	sampleCtx.Database = database
	if dbid != nil {
		sampleCtx.DBID = *dbid
	}
	sampleCtx.HasPgStatCheckpointer = hasPgStatCheckpointer
	sampleCtx.HasGenericPlan = hasGenericPlan

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
