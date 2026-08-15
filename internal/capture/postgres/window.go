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

// One connection for every sampled artifact, not one each: a pgx connection is
// not safe for concurrent use, and holding a slot per artifact for a whole
// window is the wrong thing to do to a database that may be out of them. The
// cost is that collectors run serially.

// Status values for an artifact's closing block.
const (
	// StatusComplete means every scheduled sample was written. Complete with no
	// rows is an empty database, which is a finding.
	StatusComplete = "complete"

	// StatusPartial means the window ran its course but a sample failed.
	StatusPartial = "partial"

	StatusCancelled        = "cancelled"
	StatusDeadlineExceeded = "deadline_exceeded"

	// StatusConnectFailed means the window never opened a connection. The file
	// still exists, and is the only record that the run tried.
	StatusConnectFailed = "connect_failed"
)

// Schedule is when a collector samples inside the window. The zero value is
// StartEnd.
//
// The invariant the rest of the window rests on: Every's offsets are strictly
// inside the window and StartEnd's second offset is exactly the window, so the
// closing tick is never an interval collector's. It may be shared by any number
// of start-and-end collectors, and moduleDeadline sums their budgets rather
// than assuming one owns it.
type Schedule struct {
	kind     scheduleKind
	interval time.Duration
}

type scheduleKind int

const (
	scheduleStartEnd scheduleKind = iota
	scheduleEvery
	scheduleOnce
)

func StartEnd() Schedule {
	return Schedule{kind: scheduleStartEnd}
}

func Once() Schedule {
	return Schedule{kind: scheduleOnce}
}

// Every samples from t0, the last sample one interval before the window closes
// rather than at it - see the invariant above.
func Every(d time.Duration) Schedule {
	return Schedule{kind: scheduleEvery, interval: d}
}

// offsets is when this schedule's samples are due, in order. Known before the
// window opens, which is what lets the preamble state samples_expected. A
// non-positive window or interval yields one sample at t0.
func (s Schedule) offsets(window time.Duration) []time.Duration {
	if window <= 0 {
		return []time.Duration{0}
	}

	// Before the branch below, which reads "not Every, therefore StartEnd":
	// after it, a Once collector would sample twice, the second at the closing
	// tick the invariant reserves.
	if s.kind == scheduleOnce {
		return []time.Duration{0}
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

func (s Schedule) name() string {
	switch s.kind {
	case scheduleEvery:
		return "every"

	case scheduleOnce:
		return "once"
	}

	return "start_end"
}

func (s Schedule) intervalText() string {
	if s.kind != scheduleEvery || s.interval <= 0 {
		return ""
	}

	return windowSeconds(s.interval)
}

// Artifact is what the window needs to write an artifact's preamble and closing
// block without asking the collector anything - including when the collector
// never runs at all.
type Artifact struct {
	// Name is the source= of the blocks the window writes about the artifact.
	// A collector's own blocks name what they read instead.
	Name string

	FileName string

	// Scope is "database" or "cluster" - what the block's rows are about.
	Scope string

	Schedule Schedule

	// SampleBudget is what one sample is assumed to take, which moduleDeadline
	// sums for the collectors sharing the closing tick. Zero means
	// DefaultSampleBudget. Nothing enforces it against the collector.
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

	// Sample writes one or more blocks, or writes nothing and returns an error.
	//
	// A block whose own read failed is still written - header, column header,
	// no rows - with error= in its header, so an error from here means the
	// write failed, not that any read did. The window's stub block is for a
	// collector that cannot localise a failure, and must not land behind a
	// half-written block: render the whole sample into one buffer, one Write.
	//
	// The context is the window's, not a statement's. A collector running more
	// than one statement applies StatementTimeout to each - or, where its cadence
	// makes that too long to be its bound, its own shorter one applied
	// server-side and backstopped by StatementTimeout (Sessions). The constant
	// names the default per-statement bound, not the only permitted one.
	Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
}

// Prologue is implemented by a collector carrying something knowable before the
// connection exists. Called after the preamble and before dial, so those bytes
// are on disk on every failure path.
type Prologue interface {
	WritePrologue(w io.Writer, s SampleContext) error
}

// Epilogue: called with no context; must bound its own work.
// Exists because Every's offsets stop short of the window close.
type Epilogue interface {
	WriteEpilogue(w io.Writer, s SampleContext) error
}

type SampleContext struct {
	// At is the sample's one clock read, shared by every block that sample
	// writes: equal ts= within one sample= is by construction, not a sampler
	// catching up.
	At time.Time

	Index int // 1-based: sample=1 with no sample=2 is partial

	// Total is the artifact's SamplesExpected, so a collector can write a block
	// only at the window's close - Index == Total - without knowing the
	// schedule. Zero before the first sample, as Index is.
	Total int

	// Database and DBID are current_database() and its OID. Before the
	// connection, Database is the configured name and DBID is empty.
	Database string
	DBID     string

	// HasPgStatCheckpointer is what a collector whose columns moved in
	// PostgreSQL 17 selects its statement on. A capability rather than a version
	// number, which would be wrong the moment a view is backported or a catalog
	// goes un-upgraded. Read once and shared, so two collectors cannot disagree.
	// False when identify failed.
	HasPgStatCheckpointer bool

	// redact keeps password redaction the window's one rule rather than
	// something every collector has to hold.
	redact func(error) string
}

func (s SampleContext) errorText(err error) string {
	if s.redact != nil {
		return s.redact(err)
	}

	return errorText(err, "")
}

// ArtifactResult is one artifact's outcome. Every field but IOErr is also
// written into the artifact itself; IOErr is the failure the artifact cannot
// record about itself, and the only one the adapter turns into an error.
type ArtifactResult struct {
	Artifact        Artifact
	File            *os.File
	Status          string
	SamplesExpected int
	SamplesWritten  int

	// Err is the last capture-level error, already redacted and flattened.
	Err string

	IOErr error
}

func (r ArtifactResult) writable() bool { return r.File != nil && r.IOErr == nil }

// windowConn is what the window needs of a connection. *Conn satisfies it; a
// test supplies its own and needs no server.
type windowConn interface {
	RowQuerier
	Close(ctx context.Context) error
}

// Window owns one connection and one clock for every sampled artifact in a run.
type Window struct {
	Target     Target
	Duration   time.Duration
	Collectors []Collector

	// Seams, zero in production: a clock so a test need not wait out a window, a
	// connection so it needs no server, a grace so the deadline path is
	// reachable without waiting out the real one.
	now     func() time.Time
	after   func(time.Duration) <-chan time.Time
	connect func(ctx context.Context, t Target) (windowConn, error)
	grace   time.Duration
}

// moduleDeadline is the configured duration plus what the closing tick can
// cost: the summed budgets of the collectors due there, plus room to write the
// closing block. Summed rather than a flat grace because the tick can be
// shared, so the next start-and-end artifact widens it by declaring a budget.
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

	// Nothing on the closing tick: no collectors, or every one a Once. The
	// deadline still has to cover whatever ran last.
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

// Run opens the window and returns one result per artifact.
//
// The order is the design: every file is created and its preamble synced before
// anything can fail, so a file that exists is never zero bytes for the upload
// path to drop; a refused connection writes every closing block and returns; and
// the closing blocks take no context, so an expired deadline cannot stop the
// record saying it expired.
//
// No error return - a refused connection is a successful capture of a failure.
// File I/O is the exception and lands in ArtifactResult.IOErr. Files are left
// open at their end offset for the caller to upload and close.
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
		w.closeArtifacts(results, sampleCtx, "", ConnectErrorText(err, w.Target))
		return results
	}
	defer w.disconnect(conn)

	sampleCtx = w.identify(ctx, conn)

	// Armed after dial: connecting and identifying are separately bounded, and
	// counting their 15s worst case here would spend the grace the final sample
	// needs, on exactly the slow database that needs it most.
	ctx, cancel := context.WithTimeout(ctx, w.moduleDeadline())
	defer cancel()

	stopped := w.sample(ctx, conn, sampleCtx, results)

	w.closeArtifacts(results, sampleCtx, stopped, "")

	return results
}

// openArtifacts writes each preamble, carrying the one fact a reader cannot
// reconstruct from a truncated file: how many samples were meant to be in it.
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
			// Both keys on every schedule, interval= empty where there is no
			// cadence: samples_expected does not explain itself without it.
			{"schedule", artifact.Schedule.name()},
			{"interval", artifact.Schedule.intervalText()},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
		}, w.clock())
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		syncArtifact(file)

		w.writePrologue(&results[i], w.Collectors[i], sampleCtx)
	}
}

func (w *Window) writePrologue(result *ArtifactResult, collector Collector, sampleCtx SampleContext) {
	prologue, ok := collector.(Prologue)
	if !ok {
		return
	}

	// The base context's At is the zero time, which would date the block to
	// year one. It gets the clock read the preamble header took.
	at := sampleCtx
	at.At = w.clock()

	if err := prologue.WritePrologue(result.File, at); err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", result.Artifact.FileName, err)
		return
	}

	syncArtifact(result.File)
}

// writeEpilogue: a collector's last write, guarded by writable(); no context, since closeArtifacts has none.
func (w *Window) writeEpilogue(result *ArtifactResult, collector Collector, sampleCtx SampleContext) {
	epilogue, ok := collector.(Epilogue)
	if !ok || !result.writable() {
		return
	}

	at := sampleCtx
	at.At = w.clock()

	if err := epilogue.WriteEpilogue(result.File, at); err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", result.Artifact.FileName, err)
		return
	}

	syncArtifact(result.File)
}

// closeArtifacts is the last pass. stopped is the status that ended the window
// early, or empty if it ran its course; connectErr is set only when there was
// never a connection. It takes no context on purpose: this is the record of
// what happened, including that the deadline expired.
func (w *Window) closeArtifacts(results []ArtifactResult, sampleCtx SampleContext, stopped, connectErr string) {
	// Drains run before the clock read below, so the closing timestamp doesn't predate their bytes.
	for i := range results {
		results[i].Status = artifactStatus(results[i], stopped, connectErr)

		if connectErr != "" {
			results[i].Err = connectErr
		}

		// Must run after Status is set: an epilogue IOErr makes writable() false, skipping the closing block.
		w.writeEpilogue(&results[i], w.Collectors[i], sampleCtx)
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

// artifactStatus resolves one artifact's outcome. A window stopped early says so
// whatever the counts are: "cancelled after one of two" is a different fact from
// "one of two samples failed".
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

	// collector indexes Window.Collectors, and is also the tie-break.
	collector int

	// index is that collector's own 1-based sample number: one file's sample=2
	// is unrelated to another's.
	index int
}

// timeline merges every collector's offsets into one ordered walk, which is what
// lets two cadences share one connection without either owning a clock.
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

		// Registration order, which the caller picks so a shared tick does not
		// queue a cheap read behind an expensive one.
		return cmp.Compare(a.collector, b.collector)
	})

	return events
}

// sample walks the timeline serially, returning the status that stopped the
// window early or empty if it ran to completion.
func (w *Window) sample(ctx context.Context, conn RowQuerier, sampleCtx SampleContext, results []ArtifactResult) string {
	start := w.clock()

	for _, event := range timeline(w.Collectors, w.Duration) {
		// Offsets are absolute, so one slow sample does not push the next tick
		// late. An overdue tick fires at once rather than being skipped.
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

// writeSampleError records a sample the collector could not take. A collector
// that failed has written nothing, and without this the artifact would have a
// gap in its sample numbering with nothing saying why. The block names the
// artifact rather than a view: it is a statement about the sampling.
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

// currentDatabaseSQL reads what every block header written after the connection
// names, plus the one capability a collector selects a statement on. The OID
// comes from pg_database rather than a cast of the name, so a database renamed
// mid-run still resolves.
//
// The capability expression is character-identical to serverFactsSQL's on
// purpose: pg_metadata.txt reports that flag and the statement selection reads
// this one, so two spellings could disagree about what the server has.
const currentDatabaseSQL = `SELECT current_database()::text,
       (SELECT oid::text FROM pg_catalog.pg_database WHERE datname = current_database()),
       to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL`

// identify reads the database, its OID and the capability flag once, for every
// collector. A failure falls back to the preamble's regime rather than costing
// the capture: HasPgStatCheckpointer stays false, so a PostgreSQL 17 server gets
// the pre-17 statement, which errors, which the collector degrades to a block
// carrying the reason.
func (w *Window) identify(ctx context.Context, conn RowQuerier) SampleContext {
	sampleCtx := w.baseSampleContext()

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var database string
	var dbid *string
	var hasPgStatCheckpointer bool

	if err := conn.QueryRow(stmtCtx, currentDatabaseSQL).Scan(&database, &dbid, &hasPgStatCheckpointer); err != nil {
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
		// Not `return conn, err`: a nil *Conn in a windowConn is a non-nil
		// interface, and the caller tests the interface.
		return nil, err
	}

	return conn, nil
}

// disconnect returns the connection on a fresh context. The window's own is
// routinely expired by this point, and a close that inherited it would leak the
// session it is trying to give back.
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

// stoppedStatus distinguishes the window's own deadline from the caller stopping
// it: context.WithTimeout reports its parent's cancellation as Canceled and its
// own timer as DeadlineExceeded.
func stoppedStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusDeadlineExceeded
	}

	return StatusCancelled
}

// windowSeconds renders a duration for a block header. Seconds rather than
// time.Duration's own form: 120s parses in every language the receiver might be
// written in where 2m0s does not.
func windowSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// syncArtifact flushes the artifact, ignoring a failure rather than failing the
// capture: the bytes are written either way, and the sync is insurance against
// the process not surviving to the next block.
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
