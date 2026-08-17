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

// checkpointColumns uses the pre-17 names on every version, so no mapping table is needed. Two
// reset-clock columns because PG17 split counters across two independently resettable views.
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

// checkpointSQL reads the two views PG17 split counters across (both single-row, so the cross join
// is safe). buffers_backend is a typed NULL, not dropped: 0 would wrongly mean "wrote no buffers".
const checkpointSQL = `SELECT c.num_timed,
       c.num_requested,
       c.buffers_written,
       b.buffers_clean,
       NULL::bigint AS buffers_backend,
       c.stats_reset AS checkpointer_stats_reset,
       b.stats_reset AS bgwriter_stats_reset
FROM pg_catalog.pg_stat_checkpointer c,
     pg_catalog.pg_stat_bgwriter b`

// checkpointSQLPre17 reads the same columns from the one view that held them before PG17 split it.
const checkpointSQLPre17 = `SELECT checkpoints_timed,
       checkpoints_req,
       buffers_checkpoint,
       buffers_clean,
       buffers_backend,
       stats_reset AS checkpointer_stats_reset,
       stats_reset AS bgwriter_stats_reset
FROM pg_catalog.pg_stat_bgwriter`

// connectionsSQL groups rather than filters by backend_type: parallel/autovacuum workers show as
// their own rows. ORDER BY count(*) is safe only because this block is written once, not sampled twice.
const connectionsSQL = `SELECT application_name::text,
       backend_type::text,
       count(*) AS active_connections,
       count(*) OVER () AS groups_total
FROM pg_catalog.pg_stat_activity
GROUP BY application_name, backend_type
ORDER BY count(*) DESC, application_name, backend_type
LIMIT $1`

// walSQL needs pg_monitor or superuser; a LOGIN-only role is denied and the block says so.
const walSQL = `SELECT sum(size)::bigint AS wal_bytes FROM pg_ls_waldir()`

// Capacity captures checkpoint pressure across the window, and connection distribution and WAL
// volume as it closes. Checkpoint columns are cumulative counters; deltas are the server's.
type Capacity struct {
	// MaxConnectionGroups bounds the connection block; zero takes DefaultMaxConnectionGroups.
	MaxConnectionGroups int
}

func (Capacity) Artifact() Artifact {
	return Artifact{
		Name:     "pg_capacity",
		FileName: "pg_capacity.txt",
		Scope:    "cluster",
		Schedule: StartEnd(),

		// Three statements on the closing sample; moduleDeadline sums this against other collectors.
		SampleBudget: 3 * StatementTimeout,
	}
}

// Sample writes the checkpoint block every time, and the two gauges (active_connections, wal_bytes)
// only as the window closes, since those are point-in-time readings, not deltas.
func (c Capacity) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// One buffer, one Write: avoids leaving a half-written sample if a write fails mid-block.
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

// writeCheckpointBlock writes views= whether or not the read succeeded, so it can explain the
// error beside it.
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

// writeConnectionsBlock drops the count keys on a failed read rather than writing zeroes:
// groups_total=0 would falsely assert zero connections.
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

	// NULL sum writes no row, not an empty cell: the only single-column body in the package, so an
	// empty row would be a blank line a CSV reader skips. error= distinguishes this from a failed read.
	var cells [][]string
	if err == nil && bytesWritten != nil {
		cells = [][]string{{int64Text(bytesWritten)}}
	}

	return writeRows(w, walColumns, cells)
}

// checkpointRow's columns are pointers: buffers_backend is a typed NULL from PG17 on; stats_reset
// is NULL if the server was never reset.
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

// checkpointStatement selects on the capability, not a version number, so a false positive on 17
// lands on the undefined-column error rather than a wrong answer.
func checkpointStatement(hasPgStatCheckpointer bool) string {
	if hasPgStatCheckpointer {
		return checkpointSQL
	}

	return checkpointSQLPre17
}

// checkpointViews is the block's provenance: views= varies since PG17, when three of these columns
// stopped coming from pg_stat_bgwriter.
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

// connectionRow is one (application_name, backend_type) group. backend_type is NULL when the role
// lacks pg_read_all_stats (row still counts); masking leaves application_name empty, not NULL, on 14-18.
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
