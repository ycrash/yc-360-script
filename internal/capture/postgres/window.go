package postgres

import (
	"context"
	"errors"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// One connection per timeline: pgx connections aren't safe for concurrent use.
// The window is dbcore's; this file supplies Postgres's dial, identify, error
// classes and db=/dbid=.

// engineName is engine= on every block header.
const engineName = "postgres"

const (
	ConnectionFast      = dbcore.ConnectionFast
	ConnectionNormal    = dbcore.ConnectionNormal
	ConnectionExpensive = dbcore.ConnectionExpensive
)

// Status values for an artifact's closing block; see dbcore for each one's meaning.
const (
	StatusComplete         = dbcore.StatusComplete
	StatusPartial          = dbcore.StatusPartial
	StatusCancelled        = dbcore.StatusCancelled
	StatusDeadlineExceeded = dbcore.StatusDeadlineExceeded
	StatusConnectionLost   = dbcore.StatusConnectionLost
	StatusConnectFailed    = dbcore.StatusConnectFailed
)

type (
	Schedule       = dbcore.Schedule
	Artifact       = dbcore.Artifact
	ArtifactResult = dbcore.ArtifactResult

	// Collector, Opening, Closing and SampleContext are the core's, over pgx's
	// querier and the facts identify reads.
	Collector     = dbcore.Collector[RowQuerier, Facts]
	Opening       = dbcore.Opening[Facts]
	Closing       = dbcore.Closing[Facts]
	SampleContext = dbcore.SampleContext[Facts]
)

func StartEnd() Schedule                { return dbcore.StartEnd() }
func Once() Schedule                    { return dbcore.Once() }
func Every(d time.Duration) Schedule    { return dbcore.Every(d) }
func Periodic(d time.Duration) Schedule { return dbcore.Periodic(d) }

// Facts is what identify reads beyond the core's.
type Facts struct {
	// DBID is current_database()'s OID; empty before a connection, or where
	// identify failed.
	DBID string

	// HasPgStatCheckpointer: capability check (not version) for PostgreSQL 17's moved columns; false when identify fails.
	HasPgStatCheckpointer bool
}

func artifactVersion(artifact Artifact) int { return artifact.FormatVersion() }

// windowConn is what the window needs of a connection; *Conn satisfies it, tests fake it.
type windowConn interface {
	RowQuerier
	Close(ctx context.Context) error
}

// Window owns a run's connections, one for each name its collectors' artifacts
// give, and one clock: every connection's timeline counts its offsets from the
// same start, and they run at the same time.
type Window struct {
	Target     Target
	Duration   time.Duration
	Collectors []Collector

	// CaptureID is written on every sample block of every file. A run without one
	// gets its own.
	CaptureID string

	// Test seams, zero in production: now/after skip real waits, connect skips a server, grace shortens the deadline,
	// statementClock times each sample's statements from its clock read.
	now            func() time.Time
	after          func(time.Duration) <-chan time.Time
	connect        func(ctx context.Context, t Target) (windowConn, error)
	grace          time.Duration
	statementClock func(at time.Time) func() time.Time
}

// core is the engine-neutral window this one runs as.
func (w *Window) core() *dbcore.Window[RowQuerier, Facts] {
	return &dbcore.Window[RowQuerier, Facts]{
		Engine:     w.engine(),
		Duration:   w.Duration,
		Collectors: w.Collectors,
		CaptureID:  w.CaptureID,
		TargetID:   w.Target.id(),
		Database:   w.Target.Database,
		Seams: dbcore.Seams{
			Now:            w.now,
			After:          w.after,
			Grace:          w.grace,
			StatementClock: w.statementClock,
		},
	}
}

func (w *Window) engine() dbcore.Engine[RowQuerier, Facts] {
	target := w.Target

	return dbcore.Engine[RowQuerier, Facts]{
		Name:     engineName,
		Dial:     w.dial,
		Identify: w.identifyInto,
		Location: location,

		ErrorText:        func(err error) string { return errorText(err, target.Password) },
		ConnectErrorText: func(err error) string { return ConnectErrorText(err, target) },

		// A refusal at max_connections, a role's CONNECTION LIMIT and a database's
		// are all SQLSTATE 53300 with three different fixes, told apart only by the
		// text - which is why the log keeps it in full.
		SlotRefused: func(err error) bool { return errors.Is(err, ErrTooManyConnections) },

		ReadStatus: readStatus,

		ConnectTimeout:      ConnectTimeout,
		DefaultSampleBudget: DefaultSampleBudget,
		CloseMargin:         WindowCloseMargin,
	}
}

// location is db= and dbid=: the database a block was read through, by name and OID.
func location(s SampleContext) []dbcore.HeaderField {
	return []dbcore.HeaderField{
		{Key: "db", Value: s.Database},
		{Key: "dbid", Value: s.Engine.DBID},
	}
}

// moduleDeadline: Duration + the closing tick's SampleBudgets + WindowCloseMargin.
func (w *Window) moduleDeadline() time.Duration {
	return w.core().ModuleDeadline()
}

// Run returns one result per artifact; no error return, since a refused connection is itself a captured outcome.
// Files are left open at their end offset for the caller to upload and close.
func (w *Window) Run(ctx context.Context) []ArtifactResult {
	core := w.core()
	results := core.Run(ctx)

	// The core assigns a run without one its own; the caller reads it back here.
	w.CaptureID = core.CaptureID

	return results
}

// timeline merges every collector's offsets into one ordered walk, so cadences share a connection.
func timeline(collectors []Collector, window time.Duration) []dbcore.SampleEvent {
	return dbcore.Timeline(collectors, window)
}

// OID comes from pg_database, not a name cast: survives mid-run renames.
// Capability expressions must match serverFactsSQL's exactly. server_version_num is
// written as engine_version=, never tested: capabilities come from the catalog.
const currentDatabaseSQL = `SELECT current_database()::text,
       (SELECT oid::text FROM pg_catalog.pg_database WHERE datname = current_database()),
       to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL,
       current_setting('server_version_num')`

// identify reads the database, OID and capability flags once for every collector.
// On failure, HasPgStatCheckpointer stays false, so a PG17 server gets the pre-17 statement and errors.
func (w *Window) identify(ctx context.Context, conn RowQuerier) SampleContext {
	return w.core().Identify(ctx, conn)
}

func (w *Window) identifyInto(ctx context.Context, conn RowQuerier, sampleCtx SampleContext) SampleContext {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	var database string
	var dbid *string
	var hasPgStatCheckpointer bool
	var engineVersion string

	if err := conn.QueryRow(stmtCtx, currentDatabaseSQL).
		Scan(&database, &dbid, &hasPgStatCheckpointer, &engineVersion); err != nil {
		return sampleCtx
	}

	sampleCtx.Database = database
	if dbid != nil {
		sampleCtx.Engine.DBID = *dbid
	}
	sampleCtx.Engine.HasPgStatCheckpointer = hasPgStatCheckpointer
	sampleCtx.EngineVersion = engineVersion

	return sampleCtx
}

// dial opens one connection; ctx is already bounded by ConnectTimeout.
func (w *Window) dial(ctx context.Context) (RowQuerier, dbcore.Closer, error) {
	var (
		conn windowConn
		err  error
	)

	if w.connect != nil {
		conn, err = w.connect(ctx, w.Target)
	} else {
		var real *Conn

		real, err = Connect(ctx, w.Target)
		if err == nil {
			conn = real
		}
	}

	if err != nil {
		return nil, nil, err
	}

	return conn, conn, nil
}

// stoppedStatus: context.WithTimeout reports parent cancellation as Canceled, its own timer as DeadlineExceeded.
func stoppedStatus(ctx context.Context) string { return dbcore.StoppedStatus(ctx) }

// windowSeconds renders as seconds, not time.Duration's form: 120s parses everywhere, 2m0s doesn't.
func windowSeconds(d time.Duration) string { return dbcore.WindowSeconds(d) }
