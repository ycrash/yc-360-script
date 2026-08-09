package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultMaxTables guards against a pathological schema making the artifact
// unbounded. When it fires the header says so.
const DefaultMaxTables = 10000

// bloatColumns is the server's merge contract. relid leads because it is the
// join key - relname alone is not unique across schemas.
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

// Two statements, split on cost: the size functions stat every relation's files
// on a filesystem that may itself be sick, and as one statement a large schema
// would exceed statement_timeout and lose the dead-tuple counts with it.

// bloatStatsSQL is S1. ORDER BY relid is determinism and nothing else: ordering
// on a statistic changes between the two samples by construction, and capping on
// it would hand the server two different table sets to merge.
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

// bloatSizesSQL is S2, over exactly S1's relids rather than a second scan of
// the view, which could return a different row set. The LEFT JOIN means a
// relation dropped between the two statements yields NULL sizes rather than
// putting a vanished OID into a size function.
const bloatSizesSQL = `SELECT o AS relid,
       pg_table_size(c.oid) AS table_size_bytes,
       pg_indexes_size(c.oid) AS index_size_bytes
FROM unnest($1::oid[]) AS o
LEFT JOIN pg_catalog.pg_class c ON c.oid = o`

// Bloat captures pg_stat_user_tables at both edges of the window. The ratios and
// deltas are the server's, computed by joining the two blocks on relid.
type Bloat struct {
	// MaxTables bounds one sample. Zero takes DefaultMaxTables.
	MaxTables int
}

func (Bloat) Artifact() Artifact {
	return Artifact{
		Name:     "pg_bloat",
		FileName: "pg_bloat.txt",
		Scope:    "database",
		Schedule: StartEnd(),
	}
}

// Sample runs S1, then S2 over S1's relids, and writes one block.
//
// A failed S1 returns an error having written nothing, and the window records
// the missing sample. A failed S2 is not an error: losing two columns is not
// losing the sample, so the sizes are written empty and the header says why.
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

	// One block or nothing: a write failing after the header would leave the
	// window's stub block behind a half-written body.
	var block bytes.Buffer

	// The block names the view it read; the window's own blocks name the
	// artifact.
	if err := writeBlockHeader(&block, "pg_stat_user_tables", b.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, bloatColumns, bloatCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// bloatRow is one table's row, held between the two statements so the sizes can
// be merged onto it by relid. Every counter is a pointer because 0 for idx_scan
// means "indexed and never scanned", which is a finding, where empty means "no
// indexes", which is not.
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

// readBloatSizes merges S2's two columns onto rows, in place. Nothing is
// assigned until the whole result has been read, so a failure part-way through
// leaves every row's sizes empty rather than some sized and some not.
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
		// A relid absent from the join vanished between the two statements. Its
		// sizes stay empty, which stays distinct from a relation with no storage
		// of its own - the size functions report that as 0, not NULL.
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
