// Package dbcore is the engine-neutral core of a database capture: the window,
// its schedules and the block headers. Engines supply connections, statements
// and collectors.
package dbcore

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

	"github.com/google/uuid"
)

// One connection per speed, so a slow read never delays another's samples.
const (
	ConnectionFast      = "fast"
	ConnectionNormal    = "normal"
	ConnectionExpensive = "expensive"
)

// connectionOrder: fast first, so the last free slot goes to session state.
var connectionOrder = []string{ConnectionFast, ConnectionNormal, ConnectionExpensive}

// Status values for an artifact's closing block.
const (
	// StatusComplete: an empty database still yields status=complete.
	StatusComplete = "complete"

	// StatusPartial: window ran its course but a sample failed or was skipped.
	StatusPartial = "partial"

	StatusCancelled        = "cancelled"
	StatusDeadlineExceeded = "deadline_exceeded"

	// StatusConnectionLost: the driver closed the connection mid-window; other
	// connections go on. No reconnect: it would restart every delta baseline.
	StatusConnectionLost = "connection_lost"

	// StatusConnectFailed: the file still exists, the only record the run tried.
	StatusConnectFailed = "connect_failed"
)

// Schedule: Every's offsets stay strictly inside the window, so the closing tick is never an Every collector's.
// Periodic's last offset is the close itself, so it counts towards ModuleDeadline's closing tick.
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

// Periodic is Every plus a sample at the close, so always at least two samples.
func Periodic(d time.Duration) Schedule {
	return Schedule{kind: schedulePeriodic, interval: d}
}

// Offsets are computed before the window opens, so the preamble can state samples_expected.
func (s Schedule) Offsets(window time.Duration) []time.Duration {
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

	// Drop a last step within half an interval of the close (too short a delta to
	// read), never the opening sample.
	if last := len(offsets) - 1; last > 0 && window-offsets[last] < interval/2 {
		offsets = offsets[:last]
	}

	return append(offsets, window)
}

// Name is the schedule= the preamble declares.
func (s Schedule) Name() string {
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

// IntervalText is the preamble's interval=, empty for a schedule with no cadence.
func (s Schedule) IntervalText() string {
	if (s.kind != scheduleEvery && s.kind != schedulePeriodic) || s.interval <= 0 {
		return ""
	}

	return WindowSeconds(s.interval)
}

// Artifact is what the window needs to write a preamble/closing block, even if the collector never runs.
type Artifact struct {
	// Name is the source= for window-written blocks; collector blocks name themselves.
	Name string

	FileName string

	// Scope is "database" or "cluster" - what the block's rows are about.
	Scope string

	Schedule Schedule

	// SampleBudget: assumed cost of one sample, summed across the collectors sharing a
	// connection's closing tick. Zero means the engine's DefaultSampleBudget.
	SampleBudget time.Duration

	// Format is the body format, FormatCSV when empty. Header-only blocks carry it too.
	Format string

	// Connection is the connection the artifact samples on.
	Connection string

	// Version is the file's format version, v= on each of its blocks. Zero is
	// DefaultArtifactVersion.
	Version int
}

// Periodic reports a periodic file, whose sample blocks carry capture and sample lines.
func (a Artifact) Periodic() bool { return a.Schedule.kind == schedulePeriodic }

// BodyFormat is format=, FormatCSV for an artifact that names none.
func (a Artifact) BodyFormat() string {
	if a.Format == "" {
		return FormatCSV
	}

	return a.Format
}

// FormatVersion is v=, DefaultArtifactVersion for an artifact that names none.
func (a Artifact) FormatVersion() int {
	if a.Version == 0 {
		return DefaultArtifactVersion
	}

	return a.Version
}

// Collector is one artifact's statements. Q is the engine's querier, E the facts its
// identify step reads once per connection.
type Collector[Q, E any] interface {
	Artifact() Artifact

	// Sample writes blocks or nothing+error; an error here means the write failed, not the read.
	// ctx is the window's; a collector running multiple statements bounds each itself.
	Sample(ctx context.Context, q Q, w io.Writer, s SampleContext[E]) error
}

// Opening: written after the preamble, before dial, so it lands on every failure path.
type Opening[E any] interface {
	WriteOpening(w io.Writer, s SampleContext[E]) error
}

// Closing: called with no context; must bound its own work.
// Exists because Every's offsets stop short of the window close.
type Closing[E any] interface {
	WriteClosing(w io.Writer, s SampleContext[E]) error
}

// SampleContext is what one sample knows about its run, its connection and itself.
type SampleContext[E any] struct {
	// At is one clock read shared by every block in a sample; equal ts= is by construction.
	At time.Time

	Index int // 1-based: sample=1 with no sample=2 is partial

	// Total is SamplesExpected; Index == Total marks the window's close.
	Total int

	// Database is the connected database as the server names it; pre-connect, and
	// where identify fails, it is the configured name.
	Database string

	// ConnectDuration is the cost of dialling this connection; zero if never dialled.
	ConnectDuration time.Duration

	// CaptureID is the run's, the same in every file; TargetID names the target;
	// EngineVersion is the server's version, empty when identify fails.
	CaptureID     string
	TargetID      string
	EngineVersion string

	// Engine is what identify read; zero if it never ran.
	Engine E

	// Redact renders an error for an artifact with the password removed; nil is
	// RedactError with no password.
	Redact func(error) string

	// Clock times the sample's statements; nil is the sample's own instant.
	Clock func() time.Time
}

// Now is a clock read for timing a statement; the sample's own instant where no clock
// was given.
func (s SampleContext[E]) Now() time.Time {
	if s.Clock == nil {
		return s.At
	}

	return s.Clock()
}

// ErrorText renders err for an artifact field, password redacted.
func (s SampleContext[E]) ErrorText(err error) string {
	if s.Redact != nil {
		return s.Redact(err)
	}

	return RedactError(err, "")
}

// ArtifactResult: every field but IOErr is also written into the artifact; IOErr is what it can't record about itself.
type ArtifactResult struct {
	Artifact        Artifact
	File            *os.File
	Status          string
	SamplesExpected int
	SamplesWritten  int

	// SamplesSkipped counts ticks due before the previous sample ended.
	SamplesSkipped int

	// Err is the last capture-level error, redacted; for the log only.
	Err string

	IOErr error
}

func (r ArtifactResult) writable() bool { return r.File != nil && r.IOErr == nil }

// Closer ends a connection; the window closes on a fresh context, since its own is
// usually expired by then.
type Closer interface {
	Close(ctx context.Context) error
}

// connectTimer reports a dial's cost; optional, so fakes report nothing.
type connectTimer interface {
	ConnectDuration() time.Duration
}

// connectionState reports a driver-closed connection; optional, absent means live.
type connectionState interface {
	Lost() bool
}

func connectionLost(conn Closer) bool {
	state, ok := conn.(connectionState)

	return ok && state.Lost()
}

// lossReporter is the part of a connection that kept the error the driver closed on.
type lossReporter interface {
	LossError() error
}

func connectDuration(conn Closer) time.Duration {
	if timer, ok := conn.(connectTimer); ok {
		return timer.ConnectDuration()
	}

	return 0
}

// Engine is what the window needs of a database engine to run its collectors.
type Engine[Q, E any] struct {
	// Name is engine= on every block header.
	Name string

	// Dial opens one connection; ctx is bounded by ConnectTimeout.
	Dial func(ctx context.Context) (Q, Closer, error)

	// Identify reads the connection's facts once; on failure it returns base.
	Identify func(ctx context.Context, q Q, base SampleContext[E]) SampleContext[E]

	// Location is the fields naming where a block was read (db= and the like).
	Location func(s SampleContext[E]) []HeaderField

	// ErrorText renders an error in full, password removed.
	ErrorText func(error) string

	// ConnectErrorText renders a refused dial as connect_error='s token.
	ConnectErrorText func(error) string

	// SlotRefused reports a refusal for want of a connection slot, which fails
	// only its own timeline; any other refusal fails the rest undialled.
	SlotRefused func(error) bool

	// ReadStatus is a failed sample's status=: TIMEOUT or ERROR.
	ReadStatus func(error) string

	// ConnectTimeout bounds a dial and the close of a connection.
	ConnectTimeout time.Duration

	// DefaultSampleBudget covers one sample of an artifact that declares none.
	DefaultSampleBudget time.Duration

	// CloseMargin is room to write the closing block after the last sample.
	CloseMargin time.Duration

	// KeepsFinishedOnLoss closes a file that had every sample before the loss as
	// complete rather than connection_lost.
	KeepsFinishedOnLoss bool
}

// Seams replace the window's clock and waits in tests; zero in production.
type Seams struct {
	// Now and After skip real waits.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	// Grace replaces the deadline's closing-tick budget and margin.
	Grace time.Duration

	// StatementClock times each sample's statements from its clock read.
	StatementClock func(at time.Time) func() time.Time
}

// Window owns a run's connections, one per named connection, all on one clock.
type Window[Q, E any] struct {
	Engine     Engine[Q, E]
	Duration   time.Duration
	Collectors []Collector[Q, E]

	// CaptureID is on every sample block; generated when empty.
	CaptureID string

	// TargetID names the target; Database is the configured database.
	TargetID string
	Database string

	Seams Seams
}

// ModuleDeadline: Duration + the longest connection's summed closing-tick
// SampleBudgets + CloseMargin.
func (w *Window[Q, E]) ModuleDeadline() time.Duration {
	if w.Seams.Grace > 0 {
		return w.Duration + w.Seams.Grace
	}

	budgets := map[string]time.Duration{}
	budget := time.Duration(0)

	for _, collector := range w.Collectors {
		artifact := collector.Artifact()

		offsets := artifact.Schedule.Offsets(w.Duration)
		if offsets[len(offsets)-1] == w.Duration {
			budgets[artifact.Connection] += w.sampleBudget(artifact)
			budget = max(budget, budgets[artifact.Connection])
		}
	}

	// No collectors on the closing tick: still reserve a default budget.
	if budget == 0 {
		budget = w.Engine.DefaultSampleBudget
	}

	return w.Duration + budget + w.Engine.CloseMargin
}

func (w *Window[Q, E]) sampleBudget(artifact Artifact) time.Duration {
	if artifact.SampleBudget > 0 {
		return artifact.SampleBudget
	}

	return w.Engine.DefaultSampleBudget
}

// connectionTimeline is one connection, its collectors, and how it ended.
type connectionTimeline[Q, E any] struct {
	name string

	// collectors indexes Window.Collectors, in registration order.
	collectors []int

	querier   Q
	conn      Closer
	sampleCtx SampleContext[E]

	// connectErr is the artifact's token, connectDetail the full text for the log.
	connectErr    string
	connectDetail string

	// stopped is the status that ended the timeline early; lostErr reveals a loss.
	stopped string
	lostErr string
}

// timelines groups the collectors by connection, in dialling order.
func (w *Window[Q, E]) timelines() ([]*connectionTimeline[Q, E], []*connectionTimeline[Q, E]) {
	var lines []*connectionTimeline[Q, E]

	owner := make([]*connectionTimeline[Q, E], len(w.Collectors))
	byName := map[string]*connectionTimeline[Q, E]{}

	for i, collector := range w.Collectors {
		name := collector.Artifact().Connection

		line, ok := byName[name]
		if !ok {
			line = &connectionTimeline[Q, E]{name: name}
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

	slices.SortStableFunc(lines, func(a, b *connectionTimeline[Q, E]) int {
		return cmp.Compare(rank(a.name), rank(b.name))
	})

	return lines, owner
}

// ConnectionNames is the connections these collectors open, in dialling order.
func (w *Window[Q, E]) ConnectionNames() []string {
	lines, _ := w.timelines()

	names := make([]string, len(lines))
	for i, line := range lines {
		names[i] = line.name
	}

	return names
}

// Run returns one result per artifact; no error return, since a refused connection is itself a captured outcome.
// Files are left open at their end offset for the caller to upload and close.
func (w *Window[Q, E]) Run(ctx context.Context) []ArtifactResult {
	if w.CaptureID == "" {
		w.CaptureID = uuid.NewString()
	}

	results := make([]ArtifactResult, len(w.Collectors))
	for i, collector := range w.Collectors {
		artifact := collector.Artifact()

		results[i] = ArtifactResult{
			Artifact:        artifact,
			SamplesExpected: len(artifact.Schedule.Offsets(w.Duration)),
		}
	}

	w.openArtifacts(results, w.BaseSampleContext())

	lines, owner := w.timelines()

	w.dialAll(ctx, lines)

	for _, line := range lines {
		if line.conn != nil {
			defer w.disconnect(line.conn)
		}
	}

	if slices.ContainsFunc(lines, func(line *connectionTimeline[Q, E]) bool { return line.conn != nil }) {
		w.runTimelines(ctx, lines, owner, results)
	}

	w.closeArtifacts(results, owner)

	return results
}

// runTimelines samples every connected timeline at once and returns when all have ended.
func (w *Window[Q, E]) runTimelines(
	ctx context.Context, lines, owner []*connectionTimeline[Q, E], results []ArtifactResult,
) {
	// Armed after dial, so connect/identify time doesn't eat the grace the final sample needs.
	ctx, cancel := context.WithTimeout(ctx, w.ModuleDeadline())
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

// dialAll dials each timeline in turn. A slot refusal fails only that timeline;
// any other failure is the target's and is given to the rest without dialling.
func (w *Window[Q, E]) dialAll(ctx context.Context, lines []*connectionTimeline[Q, E]) {
	var targetErr error

	for _, line := range lines {
		line.sampleCtx = w.BaseSampleContext()

		err := targetErr
		if err == nil {
			line.querier, line.conn, err = w.dial(ctx)
		}

		if err != nil {
			// Token for the artifact, full text for the log.
			var zero Q

			line.querier, line.conn = zero, nil
			line.connectErr = w.Engine.ConnectErrorText(err)
			line.connectDetail = w.errorText(err)

			if w.Engine.SlotRefused == nil || !w.Engine.SlotRefused(err) {
				targetErr = err
			}

			continue
		}

		line.sampleCtx = w.Identify(ctx, line.querier)
		line.sampleCtx.ConnectDuration = connectDuration(line.conn)
	}
}

// openArtifacts writes each preamble, including samples_expected - unrecoverable from a truncated file.
func (w *Window[Q, E]) openArtifacts(results []ArtifactResult, sampleCtx SampleContext[E]) {
	for i := range results {
		artifact := results[i].Artifact

		file, err := os.Create(artifact.FileName)
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to create %s: %w", artifact.FileName, err)
			continue
		}
		results[i].File = file

		fields := append(w.location(sampleCtx), []HeaderField{
			{"status", "started"},
			{"window", WindowSeconds(w.Duration)},
			// interval= is empty without a cadence; samples_expected needs it to make sense.
			{"schedule", artifact.Schedule.Name()},
			{"interval", artifact.Schedule.IntervalText()},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
		}...)

		err = WriteBlockHeader(file, w.Engine.Name, artifact.Name, artifact.FormatVersion(), artifact.Scope,
			artifact.BodyFormat(), fields, w.clock())
		if err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		SyncArtifact(file)

		w.writeOpening(&results[i], w.Collectors[i], sampleCtx)
	}
}

func (w *Window[Q, E]) writeOpening(result *ArtifactResult, collector Collector[Q, E], sampleCtx SampleContext[E]) {
	opening, ok := collector.(Opening[E])
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

	SyncArtifact(result.File)
}

// writeClosing: a collector's last write, guarded by writable(); no context, since closeArtifacts has none.
func (w *Window[Q, E]) writeClosing(result *ArtifactResult, collector Collector[Q, E], sampleCtx SampleContext[E]) {
	closing, ok := collector.(Closing[E])
	if !ok || !result.writable() {
		return
	}

	at := sampleCtx
	at.At = w.clock()

	if err := closing.WriteClosing(result.File, at); err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", result.Artifact.FileName, err)
		return
	}

	SyncArtifact(result.File)
}

// closeArtifacts is the last pass, with no context so it can record an expired
// deadline. Each file closes with its own connection's outcome (connection_error=,
// connect_error=).
func (w *Window[Q, E]) closeArtifacts(results []ArtifactResult, owner []*connectionTimeline[Q, E]) {
	// Drains run before the clock read below, so the closing timestamp doesn't predate their bytes.
	for i := range results {
		line := owner[i]

		results[i].Status = artifactStatus(results[i], line.stopped, line.connectErr, w.Engine.KeepsFinishedOnLoss)

		if line.connectErr != "" {
			results[i].Err = line.connectDetail
		}

		if results[i].Status == StatusConnectionLost && results[i].Err == "" {
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

		fields := append(w.location(line.sampleCtx), []HeaderField{
			{"status", results[i].Status},
			{"samples_expected", strconv.Itoa(results[i].SamplesExpected)},
			{"samples_written", strconv.Itoa(results[i].SamplesWritten)},
			{"samples_skipped", strconv.Itoa(results[i].SamplesSkipped)},
		}...)

		if line.connectErr != "" {
			fields = append(fields, HeaderField{"connect_error", line.connectErr})
		}

		if results[i].Status == StatusConnectionLost {
			fields = append(fields, HeaderField{"connection_error", line.lostErr})
		}

		artifact := results[i].Artifact
		if err := WriteBlockHeader(results[i].File, w.Engine.Name, artifact.Name, artifact.FormatVersion(),
			artifact.Scope, artifact.BodyFormat(), fields, at); err != nil {
			results[i].IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
			continue
		}

		SyncArtifact(results[i].File)
	}
}

// A stopped window overrides the sample counts: "cancelled after one of two" isn't "one of two failed".
// With keepFinished, a lost connection does not override a file that had every sample before it.
func artifactStatus(result ArtifactResult, stopped, connectErr string, keepFinished bool) string {
	switch {
	case connectErr != "":
		return StatusConnectFailed
	case keepFinished && stopped == StatusConnectionLost && result.SamplesWritten == result.SamplesExpected:
		return StatusComplete
	case stopped != "":
		return stopped
	case result.SamplesWritten == result.SamplesExpected:
		return StatusComplete
	default:
		return StatusPartial
	}
}

// SampleEvent is one tick of one collector.
type SampleEvent struct {
	At time.Duration

	// Collector indexes the collectors; also the tie-break for equal ticks.
	Collector int

	// Index is that collector's own 1-based sample number; unrelated across collectors.
	Index int
}

// Timeline merges every collector's offsets into one ordered walk, so cadences share a connection.
func Timeline[Q, E any](collectors []Collector[Q, E], window time.Duration) []SampleEvent {
	var events []SampleEvent

	for i, collector := range collectors {
		for n, at := range collector.Artifact().Schedule.Offsets(window) {
			events = append(events, SampleEvent{At: at, Collector: i, Index: n + 1})
		}
	}

	slices.SortStableFunc(events, func(a, b SampleEvent) int {
		if order := cmp.Compare(a.At, b.At); order != 0 {
			return order
		}

		// Tie-break is registration order, so a shared tick runs cheap reads before expensive ones.
		return cmp.Compare(a.Collector, b.Collector)
	})

	return events
}

// sample walks one connection's timeline serially; returns the status that stopped it
// early, or empty if complete, and for a lost connection the sample error that revealed it.
func (w *Window[Q, E]) sample(
	ctx context.Context, line *connectionTimeline[Q, E], start time.Time, owner []*connectionTimeline[Q, E],
	results []ArtifactResult,
) (stopped, lostErr string) {
	// finished is when each collector's latest sample ended.
	finished := map[int]time.Time{}
	previous := -1

	for _, event := range Timeline(w.Collectors, w.Duration) {
		if owner[event.Collector] != line {
			continue
		}

		now := w.clock()
		if previous >= 0 {
			finished[previous] = now
			previous = -1
		}

		// A stopped window counts nothing as skipped: those ticks were never going to run.
		if ctx.Err() != nil {
			return StoppedStatus(ctx), ""
		}

		// A tick due before the previous sample ended is skipped, not run late (load
		// when the database is slowest). Opening and closing samples always run.
		result := &results[event.Collector]
		if event.Index > 1 && event.Index < result.SamplesExpected &&
			start.Add(event.At).Before(finished[event.Collector]) {
			result.SamplesSkipped++

			continue
		}

		// Offsets are absolute: a slow sample doesn't delay the next tick.
		if wait := start.Add(event.At).Sub(now); wait > 0 {
			select {
			case <-ctx.Done():
				return StoppedStatus(ctx), ""
			case <-w.timer(wait):
			}
		}

		if ctx.Err() != nil {
			return StoppedStatus(ctx), ""
		}

		// A failed sample on a live connection gets a stub block; a closed
		// connection stops the timeline.
		lost, err := w.sampleOnce(ctx, line, results, event)
		previous = event.Collector

		if lost {
			// The driver also closes on a context end; report that stop instead.
			if ctx.Err() != nil {
				return StoppedStatus(ctx), ""
			}

			return StatusConnectionLost, err
		}
	}

	return "", ""
}

// sampleOnce runs one tick; lost reports a closed connection and its redacted error.
func (w *Window[Q, E]) sampleOnce(
	ctx context.Context, line *connectionTimeline[Q, E], results []ArtifactResult, event SampleEvent,
) (lost bool, err string) {
	result := &results[event.Collector]
	if !result.writable() {
		return false, ""
	}

	at := line.sampleCtx
	at.Index = event.Index
	at.Total = result.SamplesExpected
	at.At = w.clock()
	at.Clock = w.statementTimer(at.At)

	start := at.Now()

	if sampleErr := w.Collectors[event.Collector].Sample(ctx, line.querier, result.File, at); sampleErr != nil {
		w.writeSampleError(result, at, Span{start, at.Now()}, sampleErr)
	} else {
		result.SamplesWritten++
	}

	// Checked even on success: a collector may fold the error into its block.
	if !connectionLost(line.conn) {
		return false, ""
	}

	if reporter, ok := line.conn.(lossReporter); ok {
		if lossErr := w.errorText(reporter.LossError()); lossErr != "" {
			result.Err = lossErr
		}
	}

	return true, result.Err
}

// writeSampleError records a failed sample so numbering doesn't gap silently; the block names the artifact, not a view.
// In a periodic file it is a sample block like the others, timed over the whole sample.
func (w *Window[Q, E]) writeSampleError(result *ArtifactResult, sampleCtx SampleContext[E], reads Span, sampleErr error) {
	result.Err = w.errorText(sampleErr)

	artifact := result.Artifact

	var err error

	if artifact.Periodic() {
		err = WriteSampleHeader(result.File, w.Engine.Name, w.location(sampleCtx), artifact, sampleCtx, SampleHeader{
			Source: artifact.Name,
			Reads:  reads,
			Status: w.readStatus(sampleErr),
			Fields: []HeaderField{{"sample_error", result.Err}},
		})
	} else {
		fields := append(w.location(sampleCtx),
			HeaderField{"sample", strconv.Itoa(sampleCtx.Index)},
			HeaderField{"sample_error", result.Err},
		)

		err = WriteBlockHeader(result.File, w.Engine.Name, artifact.Name, artifact.FormatVersion(), artifact.Scope,
			artifact.BodyFormat(), fields, sampleCtx.At)
	}

	if err != nil {
		result.IOErr = fmt.Errorf("failed to write %s: %w", artifact.FileName, err)
	}
}

// Identify reads the connection's facts once, or returns base if the engine has none.
func (w *Window[Q, E]) Identify(ctx context.Context, q Q) SampleContext[E] {
	base := w.BaseSampleContext()

	if w.Engine.Identify == nil {
		return base
	}

	return w.Engine.Identify(ctx, q, base)
}

func (w *Window[Q, E]) dial(ctx context.Context) (Q, Closer, error) {
	connectCtx, cancel := context.WithTimeout(ctx, w.Engine.ConnectTimeout)
	defer cancel()

	return w.Engine.Dial(connectCtx)
}

// disconnect uses a fresh context: the window's is usually expired, and closing on it would leak the session.
func (w *Window[Q, E]) disconnect(conn Closer) {
	ctx, cancel := context.WithTimeout(context.Background(), w.Engine.ConnectTimeout)
	defer cancel()

	_ = conn.Close(ctx)
}

// BaseSampleContext is what every sample knows before its connection says more.
func (w *Window[Q, E]) BaseSampleContext() SampleContext[E] {
	return SampleContext[E]{
		Database:  w.Database,
		CaptureID: w.CaptureID,
		TargetID:  w.TargetID,
		Redact:    w.errorText,
	}
}

func (w *Window[Q, E]) location(s SampleContext[E]) []HeaderField {
	if w.Engine.Location == nil {
		return []HeaderField{{"db", s.Database}}
	}

	return w.Engine.Location(s)
}

func (w *Window[Q, E]) errorText(err error) string {
	if w.Engine.ErrorText == nil {
		return RedactError(err, "")
	}

	return w.Engine.ErrorText(err)
}

func (w *Window[Q, E]) readStatus(err error) string {
	if w.Engine.ReadStatus == nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return StatusTimeout
		}

		return StatusError
	}

	return w.Engine.ReadStatus(err)
}

// StoppedStatus: context.WithTimeout reports parent cancellation as Canceled, its own timer as DeadlineExceeded.
func StoppedStatus(ctx context.Context) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return StatusDeadlineExceeded
	}

	return StatusCancelled
}

// WindowSeconds renders as seconds, not time.Duration's form: 120s parses everywhere, 2m0s doesn't.
func WindowSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) + "s"
}

// SyncArtifact ignores Sync errors: bytes are already written; sync just insures against a crash before the next block.
func SyncArtifact(file *os.File) {
	_ = file.Sync()
}

func (w *Window[Q, E]) clock() time.Time {
	if w.Seams.Now != nil {
		return w.Seams.Now()
	}

	return time.Now()
}

// statementTimer is the clock a sample's statements are timed by.
func (w *Window[Q, E]) statementTimer(at time.Time) func() time.Time {
	if w.Seams.StatementClock != nil {
		return w.Seams.StatementClock(at)
	}

	return time.Now
}

func (w *Window[Q, E]) timer(d time.Duration) <-chan time.Time {
	if w.Seams.After != nil {
		return w.Seams.After(d)
	}

	return time.After(d)
}
