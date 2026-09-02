package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultMaxTables bounds one sample against a pathological schema.
const DefaultMaxTables = 10000

// relid leads: it's the join key, relname alone isn't unique across schemas.
var bloatColumns = []string{
	"relid",
	"schemaname",
	"relname",
	"n_live_tup",
	"n_dead_tup",
	"n_tup_upd",
	"n_tup_hot_upd",
	"seq_scan",
	"idx_scan",
	"last_autovacuum",
	"last_vacuum",
	"table_size_bytes",
	"index_size_bytes",
}

// Split into two statements: the size functions stat every relation's files on
// a filesystem that may itself be sick, and as one statement a large schema
// could exceed statement_timeout and lose the dead-tuple counts with it.

// ORDER BY relid for determinism only: ordering on a statistic would change
// between samples and cap on two different table sets.
const bloatStatsSQL = `SELECT relid,
       schemaname::text,
       relname::text,
       n_live_tup,
       n_dead_tup,
       n_tup_upd,
       n_tup_hot_upd,
       seq_scan,
       idx_scan,
       last_autovacuum,
       last_vacuum,
       count(*) OVER () AS tables_total
FROM pg_catalog.pg_stat_user_tables
ORDER BY relid
LIMIT $1`

// Queries S1's relids directly, not a second view scan that could return a
// different row set. LEFT JOIN: a relation dropped between statements yields
// NULL sizes rather than a vanished OID reaching a size function.
const bloatSizesSQL = `SELECT o AS relid,
       pg_table_size(c.oid) AS table_size_bytes,
       pg_indexes_size(c.oid) AS index_size_bytes
FROM unnest($1::oid[]) AS o
LEFT JOIN pg_catalog.pg_class c ON c.oid = o`

// Bloat captures pg_stat_user_tables every sample; deltas are computed
// downstream by joining consecutive blocks on relid.
type Bloat struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// MaxTables bounds one sample. Zero takes DefaultMaxTables.
	MaxTables int
}

func (b Bloat) Artifact() Artifact {
	return Artifact{
		Name:     "pg_bloat",
		FileName: "pg_bloat.txt",
		Scope:    "database",
		Schedule: Periodic(b.Interval),

		// No SampleBudget: two statements is DefaultSampleBudget already. Periodic's
		// last sample is the close, so moduleDeadline sums it there like the others.
	}
}

// Sample runs S1, then S2 over S1's relids, and writes one block. A failed S1
// errors and writes nothing; a failed S2 leaves sizes empty but still writes
// the sample.
func (b Bloat) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, total, err := b.readStats(ctx, q)
	if err != nil {
		return err
	}

	sizesErr := readBloatSizes(ctx, q, rows)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
		{"tables_written", strconv.Itoa(len(rows))},
		{"tables_total", strconv.FormatInt(total, 10)},
		{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
	}

	if sizesErr != nil {
		fields = append(fields,
			headerField{"sizes", "unavailable"},
			headerField{"reason", s.errorText(sizesErr)},
		)
	}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the view read; the window's own blocks name the artifact.
	if err := writeBlockHeader(&block, "pg_stat_user_tables", b.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, bloatColumns, bloatCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// bloatRow holds one table between the two statements so sizes can be merged
// onto it by relid. Counters are pointers: 0 for idx_scan means "indexed,
// never scanned" (a finding); nil means "no indexes".
type bloatRow struct {
	relid          uint32
	schemaName     string
	relName        string
	nLiveTup       *int64
	nDeadTup       *int64
	nTupUpd        *int64
	nTupHotUpd     *int64
	seqScan        *int64
	idxScan        *int64
	lastAutovacuum *time.Time
	lastVacuum     *time.Time

	tableSize *int64
	indexSize *int64
}

// readStats returns the capped rows and the uncapped total.
func (b Bloat) readStats(ctx context.Context, q RowQuerier) ([]bloatRow, int64, error) {
	limit := b.MaxTables
	if limit <= 0 {
		limit = DefaultMaxTables
	}

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, bloatStatsSQL, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []bloatRow
		total     int64
	)

	for rows.Next() {
		var row bloatRow

		if err := rows.Scan(
			&row.relid,
			&row.schemaName,
			&row.relName,
			&row.nLiveTup,
			&row.nDeadTup,
			&row.nTupUpd,
			&row.nTupHotUpd,
			&row.seqScan,
			&row.idxScan,
			&row.lastAutovacuum,
			&row.lastVacuum,
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

// readBloatSizes merges S2 onto rows in place. Nothing is assigned until the
// whole result is read, so a failure mid-read leaves every row's sizes empty
// rather than some sized and some not.
func readBloatSizes(ctx context.Context, q RowQuerier, rows []bloatRow) error {
	if len(rows) == 0 {
		return nil
	}

	relids := make([]uint32, len(rows))
	for i := range rows {
		relids[i] = rows[i].relid
	}

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	sized, err := q.Query(stmtCtx, bloatSizesSQL, relids)
	if err != nil {
		return err
	}
	defer sized.Close()

	type size struct{ table, index *int64 }

	sizes := make(map[uint32]size, len(rows))

	for sized.Next() {
		var (
			relid uint32
			found size
		)

		if err := sized.Scan(&relid, &found.table, &found.index); err != nil {
			return err
		}

		sizes[relid] = found
	}

	if err := sized.Err(); err != nil {
		return err
	}

	for i := range rows {
		// A relid absent vanished mid-sample: sizes stay empty, distinct from
		// "no storage", which the size functions report as 0, not NULL.
		if found, ok := sizes[rows[i].relid]; ok {
			rows[i].tableSize = found.table
			rows[i].indexSize = found.index
		}
	}

	return nil
}

func bloatCells(rows []bloatRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			strconv.FormatUint(uint64(row.relid), 10),
			row.schemaName,
			row.relName,
			int64Text(row.nLiveTup),
			int64Text(row.nDeadTup),
			int64Text(row.nTupUpd),
			int64Text(row.nTupHotUpd),
			int64Text(row.seqScan),
			int64Text(row.idxScan),
			timeText(row.lastAutovacuum),
			timeText(row.lastVacuum),
			int64Text(row.tableSize),
			int64Text(row.indexSize),
		}
	}

	return cells
}
