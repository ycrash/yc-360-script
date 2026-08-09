package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"time"
)

// DefaultHealthInterval is pg_health.txt's cadence.
const DefaultHealthInterval = 10 * time.Second

// DefaultMaxDatabases bounds one sample. The block header says when it fired.
const DefaultMaxDatabases = 1000

// undefinedColumn is the SQLSTATE for a column the server does not have. Only
// sessions_fatal can raise it here.
const undefinedColumn = "42703"

// healthColumns is the server's merge contract. datid leads because it is the
// join key - datname moves when a database is renamed mid-window. stats_reset
// closes because it is the only column that can tell a reader a delta is
// invalid: a reset is undetectable from the counters themselves.
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

// healthSQLTemplate reads pg_stat_database unfiltered - every database in the
// cluster, plus the datid=0 shared-objects row. %s is the sessions_fatal
// expression, so the fallback cannot drift from the statement it replaces.
//
// The inner ordering decides which rows survive the cap, keeping the shared row
// and the connected database: OIDs climb, so a plain ORDER BY datid would drop
// the database the block header names. The outer one is what a reader sees.
// Neither sorts on a statistic, which would hand the server a different row set
// per sample.
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

	// A NULL of the column's type, so the merged column set is identical either
	// way.
	healthSQLNoSessionsFatal = fmt.Sprintf(healthSQLTemplate, "NULL::bigint AS sessions_fatal")
)

// Health captures pg_stat_database across the window. Every column but the
// timestamps is a cumulative counter, and the arithmetic is the server's.
type Health struct {
	// Interval is the cadence. Zero takes DefaultHealthInterval.
	Interval time.Duration

	// MaxDatabases bounds one sample. Zero takes DefaultMaxDatabases.
	MaxDatabases int
}

func (h Health) Artifact() Artifact {
	return Artifact{
		Name:     "pg_health",
		FileName: "pg_health.txt",
		Scope:    "cluster",
		Schedule: Every(h.interval()),
	}
}

// Sample reads every row of pg_stat_database and writes one block.
//
// sessions_fatal arrived in PostgreSQL 14, the bottom of the supported range,
// so the 42703 retry is unreachable here and exists so an older server gets ten
// of eleven columns instead of a file of stub blocks. It is safe because each
// sample is an independent autocommit statement. The outcome is deliberately
// not remembered between samples.
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

	// One block or nothing: a write failing after the header would leave the
	// window's stub block behind a half-written body.
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

// healthRow is one database's row. datid is the join key and cannot be NULL.
// Every other scalar is a pointer so a NULL renders empty rather than costing
// the statement - datname is NULL on the shared-objects row, stats_reset on a
// database never reset - and because an empty deadlocks cell means "not read"
// where 0 means "no deadlocks".
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

// connectedOID is the OID the cap's inner ordering protects. Nil when identify
// failed, which degrades to protecting the shared row twice.
func connectedOID(dbid string) *uint32 {
	oid, err := strconv.ParseUint(dbid, 10, 32)
	if err != nil {
		return nil
	}

	value := uint32(oid)

	return &value
}

func (h Health) interval() time.Duration {
	if h.Interval <= 0 {
		return DefaultHealthInterval
	}

	return h.Interval
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
