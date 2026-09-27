package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultMaxConnectionGroups bounds the connection block, whose rows are
// (application_name, backend_type) pairs rather than applications.
const DefaultMaxConnectionGroups = 1000

// capacityVersion is pg_capacity.txt's v=: 2 when each view's checkpoint counters became a block
// under the view's own name and column names, 3 with the two-line sample header.
const capacityVersion = 3

var connectionColumns = []string{
	"application_name",
	"backend_type",
	"active_connections",
}

var walColumns = []string{"wal_bytes"}

// databaseColumns are what the cache hit, rollback and throughput ratios are read from.
var databaseColumns = []string{"xact_commit", "xact_rollback", "blks_hit", "blks_read", "temp_bytes"}

// counterBlock is one view's checkpoint counters: source= is the view, the columns are its own
// names, and the last is its stats_reset, since 17 resets the three views separately.
type counterBlock struct {
	source  string
	sql     string
	columns []string
}

// bgwriterSQLPre17 reads the one view that held every counter before 17.
const bgwriterSQLPre17 = `SELECT checkpoints_timed,
       checkpoints_req,
       buffers_checkpoint,
       buffers_clean,
       buffers_backend,
       stats_reset
FROM pg_catalog.pg_stat_bgwriter`

const checkpointerSQL = `SELECT num_timed,
       num_requested,
       buffers_written,
       stats_reset
FROM pg_catalog.pg_stat_checkpointer`

const bgwriterSQL = `SELECT buffers_clean,
       stats_reset
FROM pg_catalog.pg_stat_bgwriter`

// backendBuffersSQL is what buffers_backend counted before 17: the relation writes and extensions
// of every process but these two, which on PostgreSQL 16 matched it apart from the sync requests
// DDL makes without writing a buffer. A NULL cell is an operation its context never does; summed
// as it is, it would drop the row's writes. Every row carries the view's one reset clock.
const backendBuffersSQL = `SELECT sum(COALESCE(writes, 0) + COALESCE(extends, 0))::bigint AS buffers_backend,
       max(stats_reset) AS stats_reset
FROM pg_catalog.pg_stat_io
WHERE object = 'relation'
  AND backend_type NOT IN ('checkpointer', 'background writer')`

var checkpointBlocksPre17 = []counterBlock{{
	source: "pg_stat_bgwriter",
	sql:    bgwriterSQLPre17,
	columns: []string{
		"checkpoints_timed", "checkpoints_req", "buffers_checkpoint", "buffers_clean", "buffers_backend",
		"stats_reset",
	},
}}

// checkpointBlocks17 are the three views 17 split the counters across, each read on its own so
// one failing leaves the other two.
var checkpointBlocks17 = []counterBlock{
	{source: "pg_stat_checkpointer", sql: checkpointerSQL,
		columns: []string{"num_timed", "num_requested", "buffers_written", "stats_reset"}},
	{source: "pg_stat_bgwriter", sql: bgwriterSQL, columns: []string{"buffers_clean", "stats_reset"}},
	{source: "pg_stat_io", sql: backendBuffersSQL, columns: []string{"buffers_backend", "stats_reset"}},
}

// checkpointBlocks selects on the capability, not a version number, so a false positive on 17
// lands on the undefined-column error rather than a wrong answer.
func checkpointBlocks(hasPgStatCheckpointer bool) []counterBlock {
	if hasPgStatCheckpointer {
		return checkpointBlocks17
	}

	return checkpointBlocksPre17
}

// databaseSQL reads the connected database's row. pg_health.txt has every database's; this is the
// one this file's ratios are read from.
const databaseSQL = `SELECT xact_commit,
       xact_rollback,
       blks_hit,
       blks_read,
       temp_bytes
FROM pg_catalog.pg_stat_database
WHERE datname = current_database()`

// connectionsSQL groups rather than filters by backend_type: parallel/autovacuum workers show as
// their own rows. ORDER BY identity, never count(*): the block is sampled repeatedly under a cap,
// and a statistic ordering would let two samples keep two different group sets - the same rule
// bloatStatsSQL and pg_slow_queries.txt follow.
const connectionsSQL = `SELECT application_name::text,
       backend_type::text,
       count(*) AS active_connections,
       count(*) OVER () AS groups_total
FROM pg_catalog.pg_stat_activity
GROUP BY application_name, backend_type
ORDER BY application_name, backend_type
LIMIT $1`

// walSQL needs pg_monitor or superuser. walPrivilegeSQL is asked first, so a LOGIN-only role
// skips it and the block says reason=permission_denied.
const walSQL = `SELECT sum(size)::bigint AS wal_bytes FROM pg_ls_waldir()`

const walPrivilegeSQL = `SELECT has_function_privilege('pg_catalog.pg_ls_waldir()', 'EXECUTE')`

// Capacity captures checkpoint pressure, the connected database's throughput, connection
// distribution and WAL volume every sample. The checkpoint and database columns are cumulative
// counters and deltas are the server's; the other two are gauges, so their series is the reading
// rather than a difference between samples.
type Capacity struct {
	// Interval is the cadence, the run's normal speed (its frequency). Zero is the bookend alone.
	Interval time.Duration

	// MaxConnectionGroups bounds the connection block; zero takes DefaultMaxConnectionGroups.
	MaxConnectionGroups int
}

func (c Capacity) Artifact() Artifact {
	return Artifact{
		Name:       "pg_capacity",
		FileName:   "pg_capacity.txt",
		Scope:      "cluster",
		Schedule:   Periodic(c.Interval),
		Connection: ConnectionNormal,
		Version:    capacityVersion,

		// Seven statements on every sample from 17, five before, the WAL read's privilege
		// check among them. Periodic's last sample is the close, so moduleDeadline sums
		// this against every other closing-tick collector on the same connection.
		SampleBudget: 7 * StatementTimeout,
	}
}

// Sample writes every block every time. The gauges (active_connections, wal_bytes) used to
// land on the closing sample alone; as a series they show connections climbing and WAL growing
// through the window, which one closing reading cannot.
func (c Capacity) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// One buffer, one Write: avoids leaving a half-written sample if a write fails mid-block.
	var sample bytes.Buffer

	if err := c.writeCheckpointBlocks(ctx, q, &sample, s); err != nil {
		return err
	}

	if err := c.writeDatabaseBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	if err := c.writeConnectionsBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	if err := c.writeWALBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	_, err := w.Write(sample.Bytes())

	return err
}

func (c Capacity) writeCheckpointBlocks(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	for _, block := range checkpointBlocks(s.HasPgStatCheckpointer) {
		start := s.now()
		cells, err := readCounterBlock(ctx, q, block)
		reads := span{start, s.now()}

		if err := c.writeHeader(w, s, block.source, "", reads, err, len(cells), false, nil); err != nil {
			return err
		}

		if err := writeRows(w, block.columns, cells); err != nil {
			return err
		}
	}

	return nil
}

// writeDatabaseBlock is scope=database in a cluster file: its row is the connected database's.
func (c Capacity) writeDatabaseBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	start := s.now()
	row, err := readDatabase(ctx, q)
	reads := span{start, s.now()}

	cells := databaseCells(row)

	if err := c.writeHeader(w, s, "pg_stat_database", "database", reads, err, len(cells), false, nil); err != nil {
		return err
	}

	return writeRows(w, databaseColumns, cells)
}

// writeConnectionsBlock drops the count keys on a failed read rather than writing zeroes:
// groups_total=0 would falsely assert zero connections.
func (c Capacity) writeConnectionsBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	start := s.now()
	rows, total, err := c.readConnections(ctx, q)
	reads := span{start, s.now()}

	var fields []headerField

	if err == nil {
		fields = []headerField{
			{"groups_written", strconv.Itoa(len(rows))},
			{"groups_total", strconv.FormatInt(total, 10)},
		}
	}

	if err := c.writeHeader(w, s, "pg_stat_activity_by_app", "", reads, err, len(rows),
		int64(len(rows)) < total, fields); err != nil {
		return err
	}

	return writeRows(w, connectionColumns, connectionCells(rows))
}

func (c Capacity) writeWALBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	start := s.now()
	bytesWritten, denied, err := readWAL(ctx, q)
	reads := span{start, s.now()}

	var fields []headerField
	if denied {
		fields = []headerField{{"reason", reasonPermissionDenied}}
	}

	// NULL sum writes no row, not an empty cell: the only single-column body in the package, so an
	// empty row would be a blank line a CSV reader skips. error= and reason= distinguish this from a
	// failed or skipped read.
	var cells [][]string
	if bytesWritten != nil {
		cells = [][]string{{int64Text(bytesWritten)}}
	}

	if err := c.writeHeader(w, s, "pg_ls_waldir", "", reads, err, len(cells), false, fields); err != nil {
		return err
	}

	return writeRows(w, walColumns, cells)
}

// readCounterBlock scans one row of counters and a trailing stats_reset. A NULL is an empty cell:
// buffers_backend when pg_stat_io has no row to sum, stats_reset on a view never reset.
func readCounterBlock(ctx context.Context, q RowQuerier, block counterBlock) ([][]string, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	counters := make([]*int64, len(block.columns)-1)

	var reset *time.Time

	dest := make([]any, 0, len(block.columns))
	for i := range counters {
		dest = append(dest, &counters[i])
	}

	dest = append(dest, &reset)

	if err := q.QueryRow(stmtCtx, block.sql).Scan(dest...); err != nil {
		return nil, err
	}

	row := make([]string, 0, len(block.columns))
	for _, counter := range counters {
		row = append(row, int64Text(counter))
	}

	return [][]string{append(row, timeText(reset))}, nil
}

type databaseRow struct {
	xactCommit   *int64
	xactRollback *int64
	blksHit      *int64
	blksRead     *int64
	tempBytes    *int64
}

func readDatabase(ctx context.Context, q RowQuerier) (*databaseRow, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	var row databaseRow

	err := q.QueryRow(stmtCtx, databaseSQL).Scan(
		&row.xactCommit,
		&row.xactRollback,
		&row.blksHit,
		&row.blksRead,
		&row.tempBytes,
	)
	if err != nil {
		return nil, err
	}

	return &row, nil
}

func databaseCells(row *databaseRow) [][]string {
	if row == nil {
		return nil
	}

	return [][]string{{
		int64Text(row.xactCommit),
		int64Text(row.xactRollback),
		int64Text(row.blksHit),
		int64Text(row.blksRead),
		int64Text(row.tempBytes),
	}}
}

// connectionRow is one (application_name, backend_type) group. backend_type is NULL when the role
// lacks pg_read_all_stats (row still counts); masking leaves application_name empty, not NULL, on 14-18.
type connectionRow struct {
	applicationName *string
	backendType     *string
	connections     int64
}

func (c Capacity) readConnections(ctx context.Context, q RowQuerier) ([]connectionRow, int64, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, connectionsSQL, c.maxConnectionGroups())
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []connectionRow
		total     int64
	)

	for rows.Next() {
		var row connectionRow

		if err := rows.Scan(&row.applicationName, &row.backendType, &row.connections, &total); err != nil {
			return nil, 0, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return collected, total, nil
}

func connectionCells(rows []connectionRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			text(row.applicationName),
			text(row.backendType),
			strconv.FormatInt(row.connections, 10),
		}
	}

	return cells
}

// readWAL's sum is NULL on a directory with no files - see writeWALBlock. denied is the
// privilege check's answer; a refusal after it passed is an ordinary error.
func readWAL(ctx context.Context, q RowQuerier) (walBytes *int64, denied bool, err error) {
	allowed, err := readPrivilege(ctx, q, walPrivilegeSQL)
	if err != nil {
		return nil, false, err
	}

	if !allowed {
		return nil, true, nil
	}

	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	if err := q.QueryRow(stmtCtx, walSQL).Scan(&walBytes); err != nil {
		return nil, false, err
	}

	return walBytes, false, nil
}

// writeHeader writes one block's envelope; a failed read is its status and its error=,
// after the block's own keys.
func (c Capacity) writeHeader(w io.Writer, s SampleContext, source, scope string, reads span, err error,
	rows int, truncated bool, fields []headerField,
) error {
	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	return writeSampleHeader(w, c.Artifact(), s, sampleHeader{
		source:    source,
		scope:     scope,
		reads:     reads,
		status:    readStatus(err),
		rows:      rows,
		truncated: truncated,
		fields:    fields,
	})
}

func (c Capacity) maxConnectionGroups() int {
	if c.MaxConnectionGroups <= 0 {
		return DefaultMaxConnectionGroups
	}

	return c.MaxConnectionGroups
}
