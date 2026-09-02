package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"
)

// DefaultMaxDatabases bounds one sample.
const DefaultMaxDatabases = 1000

// undefinedColumn (42703) is the SQLSTATE sessions_fatal raises on servers older than PG14.
const undefinedColumn = "42703"

// datid, not datname, is the join key: a rename mid-window would move datname.
// stats_reset trails so a reader can detect a counter reset, undetectable from the counters alone.
var healthColumns = []string{
	"datid",
	"datname",
	"blks_hit",
	"blks_read",
	"xact_commit",
	"xact_rollback",
	"temp_files",
	"temp_bytes",
	"deadlocks",
	"sessions_fatal",
	"stats_reset",
}

// Includes the datid=0 shared-objects row. Inner ORDER BY protects that row and
// the connected database from the cap (OIDs climb); neither order sorts on a statistic, keeping the row set stable across samples.
const healthSQLTemplate = `SELECT datid,
       datname,
       blks_hit,
       blks_read,
       xact_commit,
       xact_rollback,
       temp_files,
       temp_bytes,
       deadlocks,
       sessions_fatal,
       stats_reset,
       databases_total
FROM (
  SELECT datid,
         datname::text AS datname,
         blks_hit,
         blks_read,
         xact_commit,
         xact_rollback,
         temp_files,
         temp_bytes,
         deadlocks,
         %s,
         stats_reset,
         count(*) OVER () AS databases_total
  FROM pg_catalog.pg_stat_database
  ORDER BY (datid <> 0 AND datid <> COALESCE($2::oid, 0)), datid
  LIMIT $1
) d
ORDER BY datid`

var (
	healthSQL = fmt.Sprintf(healthSQLTemplate, "sessions_fatal")

	// NULL of the right type, so the merged column set matches either way.
	healthSQLNoSessionsFatal = fmt.Sprintf(healthSQLTemplate, "NULL::bigint AS sessions_fatal")
)

// Health captures pg_stat_database each tick. Every column but the timestamps
// is a cumulative counter - the server does no delta arithmetic.
type Health struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// MaxDatabases bounds one sample. Zero takes DefaultMaxDatabases.
	MaxDatabases int
}

func (h Health) Artifact() Artifact {
	return Artifact{
		Name:     "pg_health",
		FileName: "pg_health.txt",
		Scope:    "cluster",
		Schedule: Periodic(h.Interval),

		// One statement, not DefaultSampleBudget's two. Periodic's last sample is
		// the close, so this is summed against every other closing-tick collector.
		SampleBudget: StatementTimeout,
	}
}

// Sample reads pg_stat_database and writes one block.
// sessions_fatal arrived in PG14 (our floor), so the 42703 retry below is
// currently unreachable but keeps pre-14 servers at ten of eleven columns instead of a stub block.
func (h Health) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, total, err := h.read(ctx, q, healthSQL, s)

	var withoutSessionsFatal error

	if err != nil {
		if !hasSQLState(err, undefinedColumn) {
			return err
		}

		withoutSessionsFatal = err

		if rows, total, err = h.read(ctx, q, healthSQLNoSessionsFatal, s); err != nil {
			return err
		}
	}

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
		{"databases_written", strconv.Itoa(len(rows))},
		{"databases_total", strconv.FormatInt(total, 10)},
		{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
	}

	if withoutSessionsFatal != nil {
		fields = append(fields,
			headerField{"sessions_fatal", "unavailable"},
			headerField{"reason", s.errorText(withoutSessionsFatal)},
		)
	}

	// Buffered so a header-write failure can't leave a half-written body.
	var block bytes.Buffer

	// The block names the view it read; the window's own blocks name the
	// artifact.
	if err := writeBlockHeader(&block, "pg_stat_database", h.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, healthColumns, healthCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// healthRow: datid can't be NULL (join key); other fields are pointers so NULLs
// render as empty cells - datname is NULL on the shared row; an empty deadlocks cell means "not read", not zero.
type healthRow struct {
	datid         uint32
	datName       *string
	blksHit       *int64
	blksRead      *int64
	xactCommit    *int64
	xactRollback  *int64
	tempFiles     *int64
	tempBytes     *int64
	deadlocks     *int64
	sessionsFatal *int64
	statsReset    *time.Time
}

// read returns the capped rows and the uncapped total.
func (h Health) read(ctx context.Context, q RowQuerier, sql string, s SampleContext) ([]healthRow, int64, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, sql, h.maxDatabases(), connectedOID(s.DBID))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []healthRow
		total     int64
	)

	for rows.Next() {
		var row healthRow

		if err := rows.Scan(
			&row.datid,
			&row.datName,
			&row.blksHit,
			&row.blksRead,
			&row.xactCommit,
			&row.xactRollback,
			&row.tempFiles,
			&row.tempBytes,
			&row.deadlocks,
			&row.sessionsFatal,
			&row.statsReset,
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

// connectedOID is what the cap's ordering protects; nil (identify failed) just
// protects the shared row twice.
func connectedOID(dbid string) *uint32 {
	oid, err := strconv.ParseUint(dbid, 10, 32)
	if err != nil {
		return nil
	}

	value := uint32(oid)

	return &value
}

func (h Health) maxDatabases() int {
	if h.MaxDatabases <= 0 {
		return DefaultMaxDatabases
	}

	return h.MaxDatabases
}

func healthCells(rows []healthRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			strconv.FormatUint(uint64(row.datid), 10),
			text(row.datName),
			int64Text(row.blksHit),
			int64Text(row.blksRead),
			int64Text(row.xactCommit),
			int64Text(row.xactRollback),
			int64Text(row.tempFiles),
			int64Text(row.tempBytes),
			int64Text(row.deadlocks),
			int64Text(row.sessionsFatal),
			timeText(row.statsReset),
		}
	}

	return cells
}
