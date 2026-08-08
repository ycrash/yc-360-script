package postgres

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"
)

// One connection for every sampled artifact, not one each: a pgx connection is
// not safe for concurrent use, and holding a slot per artifact for the whole
// window is the wrong thing to do to a database that may be out of them. The
// cost is that collectors run serially; every block carries its own ts=.

// Status values for an artifact's closing block.
const (
	// StatusComplete means every scheduled sample was written. A complete
	// artifact with no rows is an empty database, which is a finding.
	StatusComplete = "complete"

	// StatusPartial means the window ran its course but at least one sample
	// failed. The stub block for that sample carries the reason.
	StatusPartial = "partial"

	// StatusCancelled means the caller stopped the window early.
	StatusCancelled = "cancelled"

	// StatusDeadlineExceeded means the window's own module deadline fired.
	StatusDeadlineExceeded = "deadline_exceeded"

	// StatusConnectFailed means the window never opened a connection. The file
	// still exists, and is the only record that the run tried.
	StatusConnectFailed = "connect_failed"
)

// Schedule is when a collector samples inside the window. One variant until
// there is a second collector to justify another.
type Schedule int

// ScheduleStartEnd samples as the window opens and as it closes, so the server
// has two raw readings to difference across a known span.
const ScheduleStartEnd Schedule = iota

// samples is how many blocks the schedule produces. Known before the window
// opens, which is what lets the preamble state samples_expected - and so what
// lets a truncated file be recognised as truncated rather than read as short.
func (s Schedule) samples() int {
	switch s {
	case ScheduleStartEnd:
		return 2
	default:
		return 0
	}
}

// Artifact is what the window needs to write an artifact's preamble and
// closing block without asking the collector anything - including when the
// collector never runs at all.
type Artifact struct {
	// Name is the source= of the blocks the window writes about the artifact.
	// A collector's own sample blocks name what they read instead.
	Name string

	// FileName is the artifact in the bundle.
	FileName string

	// Scope is "database" or "cluster" - what the block's rows are about.
	Scope string

	Schedule Schedule
}

// Collector produces one artifact's sample blocks.
type Collector interface {
	Artifact() Artifact

	// Sample writes exactly one block, or writes nothing and returns an error.
	// It may not write partially: the window writes the stub block for a failed
	// sample, and cannot do that behind a half-written one.
	//
	// The context is the window's, not a statement's. A collector running more
	// than one statement applies StatementTimeout to each, so an expensive
	// statement can fail without taking a cheap one with it.
	Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
}

// SampleContext is what every sample block header needs.
type SampleContext struct {
	At    time.Time
	Index int // 1-based and monotonic: sample=1 with no sample=2 is partial

	// Database and DBID are current_database() and its OID, read once when the
	// window opens. Before that - in the preamble, and in the closing block of
	// a run that never connected - Database is the configured name and DBID is
	// empty.
	Database string
	DBID     string

	// redact renders driver text for a block header, so redaction stays the
	// window's one rule rather than a password every collector has to hold.
	redact func(error) string
}

// errorText renders err for a block header, redacted and flattened.
func (s SampleContext) errorText(err error) string {
	if s.redact != nil {
		return s.redact(err)
	}

	return errorText(err, "")
}

// ArtifactResult is one artifact's outcome. Every field but IOErr is also
// written into the artifact itself; IOErr is the failure the artifact cannot
// record about itself, and is the only one the adapter turns into an error.
type ArtifactResult struct {
	Artifact        Artifact
	File            *os.File
	Status          string
	SamplesExpected int
	SamplesWritten  int

	// Err is the last capture-level error, already redacted and flattened.
	Err string

	// IOErr is a create, write or sync failure on the file.
	IOErr error
}

// writable reports whether the artifact still has somewhere to be written.
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

	// Seams, zero in production: a clock so a test need not wait out a window,
	// a connection so it needs no server, a grace so the deadline path is
	// reachable without waiting out the real one.
	now     func() time.Time
	after   func(time.Duration) <-chan time.Time
	connect func(ctx context.Context, t Target) (windowConn, error)
	grace   time.Duration
}

// moduleDeadline bounds the sampling: the configured duration plus the grace
// the final sample runs inside. Derived here rather than taken from
// ModuleDeadline, which stays the one-shot metadata capture's.
func (w *Window) moduleDeadline() time.Duration {
	if w.grace > 0 {
		return w.Duration + w.grace
	}

	return w.Duration + WindowGrace
}

// Run opens the window and returns one result per artifact.
//
// The order is the design:
//
//  1. every file is created and its preamble written and synced before
//     anything can fail, so a file that exists is never zero bytes and the
//     upload path's empty-file check can never silently drop it;
//  2. connect. A refused connection writes every closing block and returns: a
//     run that cannot reach the database has nothing to wait for;
//  3. read the database and its OID once, for every collector;
//  4. sample, serially, on the schedule;
//  5. write every closing block with no context attached, so an expired
//     deadline cannot stop the record that says the deadline expired.
//
// There is no error return. A refused connection is a successful capture of a
// failure, and the file says so. File I/O is the exception, and lands in
// ArtifactResult.IOErr.
//
// The files are left open at their end offset. The caller uploads them - the
// upload path seeks to zero itself - and closes them.
func (w *Window) Run(ctx context.Context) []ArtifactResult {
	results := make([]ArtifactResult, len(w.Collectors))
	for i, collector := range w.Collectors {
		artifact := collector.Artifact()

		results[i] = ArtifactResult{
			Artifact:        artifact,
			SamplesExpected: artifact.Schedule.samples(),
		}
	}

	// Before a connection exists: the configured database, always non-empty
	// after Validate's defaulting, and no OID.
	sampleCtx := w.baseSampleContext()

	w.openArtifacts(results, sampleCtx)

	conn, err := w.dial(ctx)
	if err != nil {
		w.closeArtifacts(results, sampleCtx, "", ConnectErrorText(err, w.Target))
		return results
	}
	defer w.disconnect(conn)

	sampleCtx = w.identify(ctx, conn)

	// Armed here rather than before dial. Connecting and identifying are
	// separately bounded (ConnectTimeout, StatementTimeout), and counting their
	// 15s worst case against the module deadline would spend the grace the
	// final sample needs - on exactly the slow database that needs it most.
	ctx, cancel := context.WithTimeout(ctx, w.moduleDeadline())
	defer cancel()

	stopped := w.sample(ctx, conn, sampleCtx, results)

	w.closeArtifacts(results, sampleCtx, stopped, "")

	return results
}

// openArtifacts is pass one: a file per artifact, each carrying the one fact a
// reader cannot reconstruct from a truncated file - how many samples were meant
// to be in it.
func (w *Window) openArtifacts(results []ArtifactResult, sampleCtx SampleContext) {
	for i := range results {
		artifact := results[i].Artifact

		file, err := os.Create(artifact.FileName)
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to create %s: %w", artifact.FileName, err)
			continue
		}
		results[i].File = file

		err = writeBlockHeader(file, artifact.Name, artifact.Scope, []headerField{
			{"db", sampleCtx.Database},
			{"dbid", sampleCtx.DBID},
			{"status", "started"},
			{"window", windowSeconds(w.Duration)},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
		}, w.clock())
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		syncArtifact(file)
	}
}

// closeArtifacts is the last pass. stopped is the status that ended the window
// early, or empty if it ran its course; connectErr is set only when there was
// never a connection.
//
// It takes no context on purpose: this is the record of what happened,
// including the case where what happened is that the deadline expired.
func (w *Window) closeArtifacts(results []ArtifactResult, sampleCtx SampleContext, stopped, connectErr string) {
	at := w.clock()

	for i := range results {
		results[i].Status = artifactStatus(results[i], stopped, connectErr)

		if connectErr != "" {
			results[i].Err = connectErr
		}

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
		if err := writeBlockHeader(results[i].File, artifact.Name, artifact.Scope, fields, at); err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		syncArtifact(results[i].File)
	}
}

// artifactStatus resolves one artifact's outcome. A window stopped early says
// so whatever the counts are: "cancelled after one of two" is a different fact
// from "one of two samples failed".
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

// sample runs the schedule serially, returning the status that stopped the
// window early or empty if it ran to completion.
func (w *Window) sample(ctx context.Context, conn RowQuerier, sampleCtx SampleContext, results []ArtifactResult) string {
	start := w.clock()

	scheduled := 0
	for i := range results {
		scheduled = max(scheduled, results[i].SamplesExpected)
	}

	for index := 1; index <= scheduled; index++ {
		if index > 1 {
			// Targets t0+duration rather than "duration after the last sample
			// returned": sample 1's latency must not push sample 2 late,
			// because the span between the readings is what the server
			// differences across. A negative wait fires at once.
			select {
			case <-ctx.Done():
				return stoppedStatus(ctx)
			case <-w.timer(start.Add(w.Duration).Sub(w.clock())):
			}
		}

		if ctx.Err() != nil {
			return stoppedStatus(ctx)
		}

		w.sampleOnce(ctx, conn, sampleCtx, results, index)
	}

	return ""
}

// sampleOnce takes one sample from every collector due at this index.
func (w *Window) sampleOnce(ctx context.Context, conn RowQuerier, sampleCtx SampleContext, results []ArtifactResult, index int) {
	for i := range results {
		if !results[i].writable() || index > results[i].SamplesExpected {
			continue
		}

		at := sampleCtx
		at.Index = index
		at.At = w.clock()

		if err := w.Collectors[i].Sample(ctx, conn, results[i].File, at); err != nil {
			w.writeSampleError(&results[i], at, err)
			continue
		}

		results[i].SamplesWritten++
	}
}

// writeSampleError records a sample the collector could not take. The window
// writes it because a collector that failed has written nothing, and without
// this the artifact would have a gap in its sample numbering with nothing
// saying why. The block names the artifact rather than the view: it is a
// statement about the sampling.
func (w *Window) writeSampleError(result *ArtifactResult, sampleCtx SampleContext, sampleErr error) {
	result.Err = errorText(sampleErr, w.Target.Password)

	artifact := result.Artifact

	err := writeBlockHeader(result.File, artifact.Name, artifact.Scope, []headerField{
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
// names. The OID comes from pg_database rather than from a cast of the name, so
// a database renamed mid-run still resolves.
const currentDatabaseSQL = `SELECT current_database()::text,
       (SELECT oid::text FROM pg_catalog.pg_database WHERE datname = current_database())`

// identify reads the database and its OID once, for every collector. A failure
// falls back to the preamble's regime rather than costing the capture.
func (w *Window) identify(ctx context.Context, conn RowQuerier) SampleContext {
	sampleCtx := w.baseSampleContext()

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var database string
	var dbid *string

	if err := conn.QueryRow(stmtCtx, currentDatabaseSQL).Scan(&database, &dbid); err != nil {
		return sampleCtx
	}

	sampleCtx.Database = database
	if dbid != nil {
		sampleCtx.DBID = *dbid
	}

	return sampleCtx
}

// dial opens the window's one connection, bounded by ConnectTimeout: a database
// that refuses must not cost the capture the wall clock it was going to spend
// sampling.
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

// baseSampleContext is what every collector is handed before the connection has
// said otherwise.
func (w *Window) baseSampleContext() SampleContext {
	password := w.Target.Password

	return SampleContext{
		Database: w.Target.Database,
		redact:   func(err error) string { return errorText(err, password) },
	}
}

// stoppedStatus distinguishes the window's own deadline from the caller
// stopping it: context.WithTimeout reports its parent's cancellation as
// Canceled and its own timer as DeadlineExceeded.
func stoppedStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusDeadlineExceeded
	}

	return StatusCancelled
}

// windowSeconds renders the window for a block header. Seconds rather than
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
