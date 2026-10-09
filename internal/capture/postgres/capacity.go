package postgres

import (
	"bytes"
	"context"
	"io"
	"time"
)

// capacityVersion is pg_capacity.txt's v=: 2 when each view's checkpoint counters became a block
// under the view's own name and column names, 3 with the two-line sample header, 4 when the
// connection block left the file (the server counts connections from pg_sessions.txt).
const capacityVersion = 4

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

// walSQL needs pg_monitor or superuser. walPrivilegeSQL is asked first, so a LOGIN-only role
// skips it and the block says reason=permission_denied.
const walSQL = `SELECT sum(size)::bigint AS wal_bytes FROM pg_ls_waldir()`

const walPrivilegeSQL = `SELECT has_function_privilege('pg_catalog.pg_ls_waldir()', 'EXECUTE')`

// Capacity captures checkpoint pressure, the connected database's throughput and WAL volume
// every sample. The checkpoint and database columns are cumulative counters and deltas are the
// server's; wal_bytes is a gauge, so its series is the reading rather than a difference.
type Capacity struct {
	// Interval is the cadence, the run's normal speed (its frequency). Zero is the bookend alone.
	Interval time.Duration
}

func (c Capacity) Artifact() Artifact {
	return Artifact{
		Name:       "pg_capacity",
		FileName:   "pg_capacity.txt",
		Scope:      "cluster",
		Schedule:   Periodic(c.Interval),
		Connection: ConnectionNormal,
		Version:    capacityVersion,

		// Six statements on every sample from 17, four before, the WAL read's privilege
		// check among them. Periodic's last sample is the close, so moduleDeadline sums
		// this against every other closing-tick collector on the same connection.
		SampleBudget: 6 * StatementTimeout,
	}
}

// Sample writes every block every time. wal_bytes used to land on the closing sample alone; as
// a series it shows WAL growing through the window, which one closing reading cannot.
func (c Capacity) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// One buffer, one Write: avoids leaving a half-written sample if a write fails mid-block.
	var sample bytes.Buffer

	if err := c.writeCheckpointBlocks(ctx, q, &sample, s); err != nil {
		return err
	}

	if err := c.writeDatabaseBlock(ctx, q, &sample, s); err != nil {
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

		if err := c.writeHeader(w, s, block.source, "", reads, err, len(cells), nil); err != nil {
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

	if err := c.writeHeader(w, s, "pg_stat_database", "database", reads, err, len(cells), nil); err != nil {
		return err
	}

	return writeRows(w, databaseColumns, cells)
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

	if err := c.writeHeader(w, s, "pg_ls_waldir", "", reads, err, len(cells), fields); err != nil {
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
	rows int, fields []headerField,
) error {
	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	return writeSampleHeader(w, c.Artifact(), s, sampleHeader{
		source: source,
		scope:  scope,
		reads:  reads,
		status: readStatus(err),
		rows:   rows,
		fields: fields,
	})
}
