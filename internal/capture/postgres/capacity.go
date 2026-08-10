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

// checkpointColumns is the server's merge contract: the pre-17 names on every
// version, so a receiver carries no mapping table. Two reset clocks because
// PostgreSQL 17 split the counters across two independently resettable views,
// where one clock would hide the other view's reset; below 17 they are one
// column read twice.
var checkpointColumns = []string{
	"checkpoints_timed",
	"checkpoints_req",
	"buffers_checkpoint",
	"buffers_clean",
	"buffers_backend",
	"checkpointer_stats_reset",
	"bgwriter_stats_reset",
}

var connectionColumns = []string{
	"application_name",
	"backend_type",
	"active_connections",
}

var walColumns = []string{"wal_bytes"}

// checkpointSQL reads the two views PostgreSQL 17 split the counters across;
// both return one row, so the cross join returns one. buffers_backend is a typed
// NULL rather than a dropped column, so the column set is identical on every
// version: 0 would mean backends wrote no buffers, empty means 17 stopped
// counting.
const checkpointSQL = `SELECT c.num_timed,
       c.num_requested,
       c.buffers_written,
       b.buffers_clean,
       NULL::bigint AS buffers_backend,
       c.stats_reset AS checkpointer_stats_reset,
       b.stats_reset AS bgwriter_stats_reset
FROM pg_catalog.pg_stat_checkpointer c,
     pg_catalog.pg_stat_bgwriter b`

// checkpointSQLPre17 reads the same column set from the one view that held it
// before the split.
const checkpointSQLPre17 = `SELECT checkpoints_timed,
       checkpoints_req,
       buffers_checkpoint,
       buffers_clean,
       buffers_backend,
       stats_reset AS checkpointer_stats_reset,
       stats_reset AS bgwriter_stats_reset
FROM pg_catalog.pg_stat_bgwriter`

// connectionsSQL counts the processes that exist as the window closes.
//
// backend_type groups rather than filters: whether the extra rows are parallel
// workers or autovacuum workers is a finding. The agent's own session stays in,
// so the block agrees with a hand-run count(*).
//
// Ordering on a statistic is a ranking, which the other start-and-end collectors
// forbid. Safe only because this block is written once; a second sample would
// have to go back to a stable key.
const connectionsSQL = `SELECT application_name::text,
       backend_type::text,
       count(*) AS active_connections,
       count(*) OVER () AS groups_total
FROM pg_catalog.pg_stat_activity
GROUP BY application_name, backend_type
ORDER BY count(*) DESC, application_name, backend_type
LIMIT $1`

// walSQL needs pg_monitor or superuser. A role holding only LOGIN is denied,
// and the block then says so rather than the artifact failing.
const walSQL = `SELECT sum(size)::bigint AS wal_bytes FROM pg_ls_waldir()`

// Capacity captures checkpoint pressure across the window, and the connection
// distribution and WAL volume as it closes. The checkpoint columns are
// cumulative counters; the deltas are the server's.
type Capacity struct {
	// MaxConnectionGroups bounds the connection block. Zero takes
	// DefaultMaxConnectionGroups.
	MaxConnectionGroups int
}

func (Capacity) Artifact() Artifact {
	return Artifact{
		Name:     "pg_capacity",
		FileName: "pg_capacity.txt",
		Scope:    "cluster",
		Schedule: StartEnd(),

		// Three statements on the closing sample, which moduleDeadline sums
		// against the other collectors due there.
		SampleBudget: 3 * StatementTimeout,
	}
}

// Sample writes the checkpoint block every time and the two gauges only as the
// window closes: active_connections and wal_bytes are readings of what exists
// now, so a start-and-end pair would be two unrelated numbers, not a delta.
// Index == Total also holds for a degenerate window's single sample.
//
// Each block turns its own read failure into an error= header and an empty body,
// so an error means the write failed, not that any read did.
func (c Capacity) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// N blocks, one buffer, one Write: a write failing between two of them would
	// leave the window's stub block behind a half-written sample.
	var sample bytes.Buffer

	if err := c.writeCheckpointBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	if s.Index == s.Total {
		if err := c.writeConnectionsBlock(ctx, q, &sample, s); err != nil {
			return err
		}

		if err := c.writeWALBlock(ctx, q, &sample, s); err != nil {
			return err
		}
	}

	_, err := w.Write(sample.Bytes())

	return err
}

// writeCheckpointBlock writes views= whether or not the read succeeded: which
// variant was attempted is what explains the error beside it.
func (c Capacity) writeCheckpointBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	row, err := readCheckpoint(ctx, q, s.HasPgStatCheckpointer)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
		{"views", checkpointViews(s.HasPgStatCheckpointer)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	if err := writeBlockHeader(w, "pg_checkpointer", c.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	return writeRows(w, checkpointColumns, checkpointCells(row))
}

// writeConnectionsBlock drops the count keys on a failed read rather than
// writing zeroes: groups_total=0 would assert that the server has no
// connections, where the truth is that nobody could count them.
func (c Capacity) writeConnectionsBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, total, err := c.readConnections(ctx, q)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	} else {
		fields = append(fields,
			headerField{"groups_written", strconv.Itoa(len(rows))},
			headerField{"groups_total", strconv.FormatInt(total, 10)},
			headerField{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
		)
	}

	if err := writeBlockHeader(w, "pg_stat_activity_by_app", c.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	return writeRows(w, connectionColumns, connectionCells(rows))
}

func (c Capacity) writeWALBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	bytesWritten, err := readWAL(ctx, q)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	if err := writeBlockHeader(w, "pg_ls_waldir", c.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	// A NULL sum writes no row rather than one empty cell: this is the only
	// single-column body in the package, so an all-empty row is a blank line a
	// CSV reader skips. error= is what separates that from a failed read.
	var cells [][]string
	if err == nil && bytesWritten != nil {
		cells = [][]string{{int64Text(bytesWritten)}}
	}

	return writeRows(w, walColumns, cells)
}

// checkpointRow's columns are all pointers: buffers_backend is a typed NULL from
// 17 on, and stats_reset is NULL on a server never reset.
type checkpointRow struct {
	checkpointsTimed  *int64
	checkpointsReq    *int64
	buffersCheckpoint *int64
	buffersClean      *int64
	buffersBackend    *int64
	checkpointerReset *time.Time
	bgwriterReset     *time.Time
}

func readCheckpoint(ctx context.Context, q RowQuerier, hasPgStatCheckpointer bool) (*checkpointRow, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var row checkpointRow

	err := q.QueryRow(stmtCtx, checkpointStatement(hasPgStatCheckpointer)).Scan(
		&row.checkpointsTimed,
		&row.checkpointsReq,
		&row.buffersCheckpoint,
		&row.buffersClean,
		&row.buffersBackend,
		&row.checkpointerReset,
		&row.bgwriterReset,
	)
	if err != nil {
		return nil, err
	}

	return &row, nil
}

// checkpointStatement selects on the capability, not a version number. False on
// a 17 server is the identify fallback, and lands on a block carrying the
// undefined-column error rather than on a wrong answer.
func checkpointStatement(hasPgStatCheckpointer bool) string {
	if hasPgStatCheckpointer {
		return checkpointSQL
	}

	return checkpointSQLPre17
}

// checkpointViews is the block's provenance. source= is the parser's dispatch
// key and must be stable across versions; views= has to vary, because from 17 on
// three of these columns do not come from pg_stat_bgwriter at all.
func checkpointViews(hasPgStatCheckpointer bool) string {
	if hasPgStatCheckpointer {
		return "pg_stat_checkpointer,pg_stat_bgwriter"
	}

	return "pg_stat_bgwriter"
}

func checkpointCells(row *checkpointRow) [][]string {
	if row == nil {
		return nil
	}

	return [][]string{{
		int64Text(row.checkpointsTimed),
		int64Text(row.checkpointsReq),
		int64Text(row.buffersCheckpoint),
		int64Text(row.buffersClean),
		int64Text(row.buffersBackend),
		timeText(row.checkpointerReset),
		timeText(row.bgwriterReset),
	}}
}

// connectionRow is one (application_name, backend_type) group. backend_type is
// NULL for a backend the role cannot see without pg_read_all_stats: the row
// still counts, so the total stays right while the grain collapses, which is a
// finding about the grant. application_name is empty there, and for a connection
// that never set one - masking leaves it empty rather than NULL on 14 to 18.
type connectionRow struct {
	applicationName *string
	backendType     *string
	connections     int64
}

func (c Capacity) readConnections(ctx context.Context, q RowQuerier) ([]connectionRow, int64, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
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

// readWAL's sum is NULL on a directory with no files - see writeWALBlock.
func readWAL(ctx context.Context, q RowQuerier) (*int64, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var walBytes *int64

	if err := q.QueryRow(stmtCtx, walSQL).Scan(&walBytes); err != nil {
		return nil, err
	}

	return walBytes, nil
}

func (c Capacity) maxConnectionGroups() int {
	if c.MaxConnectionGroups <= 0 {
		return DefaultMaxConnectionGroups
	}

	return c.MaxConnectionGroups
}
