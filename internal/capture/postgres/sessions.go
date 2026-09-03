package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"
)

// SessionsStatementTimeout is applied server-side (SET/RESET), never as a context deadline:
// pgx closes the connection on context expiry, and this window never reconnects.
const SessionsStatementTimeout = 1500 * time.Millisecond

// setSessionsTimeoutSQL is formatted from the constant so the literal can't drift.
// RESET restores the startup-packet value rather than restating it.
var setSessionsTimeoutSQL = fmt.Sprintf("SET statement_timeout TO '%dms'",
	SessionsStatementTimeout.Milliseconds())

const resetSessionsTimeoutSQL = `RESET statement_timeout`

// DefaultMaxSessions and DefaultMaxLocks bound one sample's rows;
// DefaultMaxQueryText bounds query text in runes (8x the server's default, well under its 1MB ceiling).
const (
	DefaultMaxSessions  = 1000
	DefaultMaxLocks     = 5000
	DefaultMaxQueryText = 8192
)

// sessionColumns: pid leads as the stitch key (also guaranteeing no data line can start with
// '#', since the first cell is always an integer); query closes as the only unbounded column.
var sessionColumns = []string{
	"pid",
	"datid",
	"datname",
	"leader_pid",
	"usesysid",
	"usename",
	"application_name",
	"backend_type",
	"state",
	"wait_event_type",
	"wait_event",
	"backend_start",
	"xact_start",
	"query_start",
	"state_change",
	"backend_xid",
	"backend_xmin",
	"query_id",
	"client_addr",
	"client_hostname",
	"client_port",
	"query",
}

// sessionsSQL reads pg_stat_activity with no WHERE clause: filtering (e.g. by backend_type)
// would silently mask rows for a role without pg_read_all_stats.
// oid/xid columns are cast; pgx has no scan plan for them, and host(client_addr) avoids inet's ::text /32 suffix.
const sessionsSQL = `SELECT pid,
       datid::text,
       datname::text,
       leader_pid,
       usesysid::text,
       usename::text,
       application_name,
       backend_type,
       state,
       wait_event_type,
       wait_event,
       backend_start,
       xact_start,
       query_start,
       state_change,
       backend_xid::text,
       backend_xmin::text,
       query_id,
       host(client_addr),
       client_hostname,
       client_port,
       left(query, $2) AS query,
       count(*) OVER () AS sessions_total
FROM pg_catalog.pg_stat_activity
ORDER BY pid
LIMIT $1`

// lockColumns: pid leads as the join key (NULL only for a prepared transaction's anonymous locks).
// waitstart closes; it can lag a wait's start briefly, so granted=false is the "is waiting" marker.
var lockColumns = []string{
	"pid",
	"locktype",
	"database",
	"relation",
	"page",
	"tuple",
	"virtualxid",
	"transactionid",
	"classid",
	"objid",
	"objsubid",
	"virtualtransaction",
	"mode",
	"granted",
	"fastpath",
	"waitstart",
}

// locksSQL reads pg_locks (no grant needed, unlike pg_stat_activity: LOGIN alone sees every row/column).
// OIDs are captured raw, never resolved via regclass (wrong on a foreign database's OID, measured on 18).
// ORDER BY is the full lock identity plus virtualtransaction, never NULL, for determinism
// (advisory/prepared-transaction locks can tie otherwise); granted is excluded since it changes between samples.
const locksSQL = `SELECT pid,
       locktype,
       database::text,
       relation::text,
       page,
       tuple,
       virtualxid,
       transactionid::text,
       classid::text,
       objid::text,
       objsubid,
       virtualtransaction,
       mode,
       granted,
       fastpath,
       waitstart,
       count(*) OVER () AS locks_total
FROM pg_catalog.pg_locks
ORDER BY pid, locktype, database, relation, page, tuple,
         virtualxid, transactionid::text, classid, objid, objsubid, mode,
         virtualtransaction
LIMIT $1`

// Sessions captures pg_stat_activity and pg_locks: what each session is running, and what each is waiting for.
// query masks to the literal "<insufficient privilege>" rather than NULL without pg_read_all_stats,
// so a least-privilege capture looks complete with every query cell a denial;
// pg_metadata.txt's has_pg_read_all_stats is what tells the two apart.
type Sessions struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// MaxSessions bounds one sample's activity rows. Zero takes
	// DefaultMaxSessions.
	MaxSessions int

	// MaxLocks bounds one sample's lock rows. Zero takes DefaultMaxLocks.
	MaxLocks int
}

func (s Sessions) Artifact() Artifact {
	return Artifact{
		Name:     "pg_sessions",
		FileName: "pg_sessions.txt",
		Scope:    "cluster",
		Schedule: Periodic(s.Interval),

		// Periodic's last sample is the close, so moduleDeadline sums this one.
		// The SET in Sample is what enforces the timeout.
		SampleBudget: 2 * SessionsStatementTimeout,
	}
}

// Sample reads pg_stat_activity then pg_locks, writing one block each; a returned error means
// the write failed, not either read. Both blocks share one ts= (one clock read per sample).
func (s Sessions) Sample(ctx context.Context, q RowQuerier, w io.Writer, sc SampleContext) error {
	// One buffer, one Write: a partial failure would otherwise leave a half-written sample.
	var sample bytes.Buffer

	setSessionsTimeout(ctx, q)
	defer resetSessionsTimeout(ctx, q)

	if err := s.writeSessionsBlock(ctx, q, &sample, sc); err != nil {
		return err
	}

	if err := s.writeLocksBlock(ctx, q, &sample, sc); err != nil {
		return err
	}

	_, err := w.Write(sample.Bytes())

	return err
}

func (s Sessions) writeSessionsBlock(ctx context.Context, q RowQuerier, w io.Writer, sc SampleContext) error {
	rows, total, err := s.readSessions(ctx, q)

	cells, queriesTruncated := sessionCells(rows)

	fields := []headerField{
		{"db", sc.Database},
		{"dbid", sc.DBID},
		{"sample", strconv.Itoa(sc.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", sc.errorText(err)})
	} else {
		fields = append(fields,
			headerField{"sessions_written", strconv.Itoa(len(rows))},
			headerField{"sessions_total", strconv.FormatInt(total, 10)},
			headerField{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
		)

		if queriesTruncated > 0 {
			fields = append(fields, headerField{"queries_truncated", strconv.Itoa(queriesTruncated)})
		}
	}

	if err := writeBlockHeader(w, "pg_stat_activity", s.Artifact().Scope, fields, sc.At); err != nil {
		return err
	}

	return writeRows(w, sessionColumns, cells)
}

func (s Sessions) writeLocksBlock(ctx context.Context, q RowQuerier, w io.Writer, sc SampleContext) error {
	rows, total, err := s.readLocks(ctx, q)

	fields := []headerField{
		{"db", sc.Database},
		{"dbid", sc.DBID},
		{"sample", strconv.Itoa(sc.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", sc.errorText(err)})
	} else {
		fields = append(fields,
			headerField{"locks_written", strconv.Itoa(len(rows))},
			headerField{"locks_total", strconv.FormatInt(total, 10)},
			headerField{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
		)
	}

	if err := writeBlockHeader(w, "pg_locks", s.Artifact().Scope, fields, sc.At); err != nil {
		return err
	}

	return writeRows(w, lockColumns, lockCells(rows))
}

// pid is a non-pointer: pg_stat_activity never NULLs it, even under masking (measured on 14, 18).
// Every other field is a pointer; NULL means "not applicable" or "masked," never zero
// (client_port is -1, not NULL, on a unix-socket connection).
type sessionRow struct {
	pid             int32
	datid           *string
	datName         *string
	leaderPID       *int32
	usesysid        *string
	usename         *string
	applicationName *string
	backendType     *string
	state           *string
	waitEventType   *string
	waitEvent       *string
	backendStart    *time.Time
	xactStart       *time.Time
	queryStart      *time.Time
	stateChange     *time.Time
	backendXID      *string
	backendXmin     *string
	queryID         *int64
	clientAddr      *string
	clientHostname  *string
	clientPort      *int32
	query           *string
}

func (s Sessions) readSessions(ctx context.Context, q RowQuerier) ([]sessionRow, int64, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	// left() is asked for one rune past the cap so sessionCells can tell the two apart:
	// fetched at exactly the cap, a query of that length and a truncated one are identical
	// and queries_truncated= would always be 0.
	rows, err := q.Query(stmtCtx, sessionsSQL, s.maxSessions(), DefaultMaxQueryText+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []sessionRow
		total     int64
	)

	for rows.Next() {
		var row sessionRow

		if err := rows.Scan(
			&row.pid,
			&row.datid,
			&row.datName,
			&row.leaderPID,
			&row.usesysid,
			&row.usename,
			&row.applicationName,
			&row.backendType,
			&row.state,
			&row.waitEventType,
			&row.waitEvent,
			&row.backendStart,
			&row.xactStart,
			&row.queryStart,
			&row.stateChange,
			&row.backendXID,
			&row.backendXmin,
			&row.queryID,
			&row.clientAddr,
			&row.clientHostname,
			&row.clientPort,
			&row.query,
			&total,
		); err != nil {
			return nil, 0, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return collected, total, nil
}

func sessionCells(rows []sessionRow) ([][]string, int) {
	cells := make([][]string, len(rows))
	truncated := 0

	for i, row := range rows {
		query := text(row.query)

		// truncateRunes returns the value unchanged when under the limit, so this can't drift from it.
		if capped := truncateRunes(query, DefaultMaxQueryText); capped != query {
			query = capped
			truncated++
		}

		cells[i] = []string{
			strconv.FormatInt(int64(row.pid), 10),
			text(row.datid),
			text(row.datName),
			int32Text(row.leaderPID),
			text(row.usesysid),
			text(row.usename),
			text(row.applicationName),
			text(row.backendType),
			text(row.state),
			text(row.waitEventType),
			text(row.waitEvent),
			timeText(row.backendStart),
			timeText(row.xactStart),
			timeText(row.queryStart),
			timeText(row.stateChange),
			text(row.backendXID),
			text(row.backendXmin),
			int64Text(row.queryID),
			text(row.clientAddr),
			text(row.clientHostname),
			int32Text(row.clientPort),
			query,
		}
	}

	return cells, truncated
}

// pid is a pointer here, unlike sessionRow's: pg_locks.pid is NULL for a prepared transaction's
// locks (measured on 18). Other fields are pointers too; NULL varies by lock type, not zero.
type lockRow struct {
	pid                *int32
	locktype           *string
	database           *string
	relation           *string
	page               *int32
	tuple              *int32
	virtualxid         *string
	transactionID      *string
	classid            *string
	objid              *string
	objsubid           *int32
	virtualtransaction *string
	mode               *string
	granted            *bool
	fastpath           *bool
	waitstart          *time.Time
}

func (s Sessions) readLocks(ctx context.Context, q RowQuerier) ([]lockRow, int64, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, locksSQL, s.maxLocks())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []lockRow
		total     int64
	)

	for rows.Next() {
		var row lockRow

		if err := rows.Scan(
			&row.pid,
			&row.locktype,
			&row.database,
			&row.relation,
			&row.page,
			&row.tuple,
			&row.virtualxid,
			&row.transactionID,
			&row.classid,
			&row.objid,
			&row.objsubid,
			&row.virtualtransaction,
			&row.mode,
			&row.granted,
			&row.fastpath,
			&row.waitstart,
			&total,
		); err != nil {
			return nil, 0, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return collected, total, nil
}

func lockCells(rows []lockRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			int32Text(row.pid),
			text(row.locktype),
			text(row.database),
			text(row.relation),
			int32Text(row.page),
			int32Text(row.tuple),
			text(row.virtualxid),
			text(row.transactionID),
			text(row.classid),
			text(row.objid),
			int32Text(row.objsubid),
			text(row.virtualtransaction),
			text(row.mode),
			boolText(row.granted),
			boolText(row.fastpath),
			timeText(row.waitstart),
		}
	}

	return cells
}

// setSessionsTimeout ignores its own failure: a failed SET just leaves the sample at the
// package's slower timeout, and the reads beside it already report a lost connection.
func setSessionsTimeout(ctx context.Context, q RowQuerier) {
	runUtilityStatement(ctx, q, setSessionsTimeoutSQL)
}

// resetSessionsTimeout always runs, even after a failed read, so the shared connection is
// handed back with its default. Uses the sample's own context, so an expired window skips it.
func resetSessionsTimeout(ctx context.Context, q RowQuerier) {
	runUtilityStatement(ctx, q, resetSessionsTimeoutSQL)
}

// runUtilityStatement uses Query (RowQuerier has no Exec) and drains the result: an undrained
// row leaves the connection "busy" and fails the next statement.
func runUtilityStatement(ctx context.Context, q RowQuerier, sql string) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, sql)
	if err != nil {
		return
	}
	defer rows.Close()

	for rows.Next() {
	}
}

func (s Sessions) maxSessions() int {
	if s.MaxSessions <= 0 {
		return DefaultMaxSessions
	}

	return s.MaxSessions
}

func (s Sessions) maxLocks() int {
	if s.MaxLocks <= 0 {
		return DefaultMaxLocks
	}

	return s.MaxLocks
}
