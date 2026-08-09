package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultMaxTables bounds the rows one sample writes: a guard against a
// pathological schema making the artifact unbounded, not a choice of
// interesting tables. When it fires the header says so, so a capped file cannot
// read as a complete one. Not configurable.
const DefaultMaxTables = 10000

// bloatColumns is the server's merge contract, not a presentation choice. relid
// leads because it is the join key the two samples are merged on - relname
// alone is not unique across schemas. Nothing here may be reordered without the
// receiver agreeing to it.
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

// The sample is two statements, split on cost. Reading pg_stat_user_tables is a
// catalogue read; pg_table_size and pg_indexes_size stat each relation's files,
// on a filesystem that during these incidents may be the thing that is sick. As
// one statement a large schema can exceed statement_timeout and lose the
// dead-tuple counts too, which are what the report is built from. Split, a
// failed S2 costs two columns and says so in the header.

// bloatStatsSQL is S1. ORDER BY relid is determinism and nothing else: ordering
// by a statistic changes between the two samples by construction, and capping
// on it would hand the server two different table sets to merge. count(*) OVER
// () is what lets the header state what the cap dropped.
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
// the view: re-scanning can return a different row set, so the merge back onto
// S1's rows would be partial in a way nothing records. The LEFT JOIN means a
// relation dropped between the two statements yields NULL sizes rather than
// putting a vanished OID into a size function.
const bloatSizesSQL = `SELECT o AS relid,
       pg_table_size(c.oid) AS table_size_bytes,
       pg_indexes_size(c.oid) AS index_size_bytes
FROM unnest($1::oid[]) AS o
LEFT JOIN pg_catalog.pg_class c ON c.oid = o`

// Bloat captures pg_stat_user_tables at the start and the end of the window and
// derives nothing: the ratios and deltas are the server's, computed by joining
// the two blocks on relid.
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

// Sample runs S1, then S2 over S1's relids, merges the sizes onto S1's rows in
// memory, and writes one block.
//
// A failed S1 returns an error having written nothing, and the window records
// the missing sample. A failed S2 is not an error: the sizes are written empty
// and the header says why, because losing two columns is not losing the sample.
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

	// Rendered whole, then written once: the contract is one block or nothing,
	// and a write failing after the header would leave the window's stub block
	// behind a half-written body.
	var block bytes.Buffer

	// The block names the view it read, not the artifact: this is what was
	// captured, where the window's own blocks are about the capturing.
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
// be merged onto it by relid.
//
// Every counter is a pointer. The distinction is the artifact's, not Go's: 0
// for idx_scan means "indexed and never scanned", which is a finding, and empty
// means "no indexes", which is not.
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

// readStats runs S1, returning the capped rows and the uncapped total.
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

// readBloatSizes runs S2 and merges its two columns onto rows, in place.
//
// Nothing is assigned until the whole result has been read, so a failure
// part-way through leaves every row's sizes empty rather than some rows sized
// and some not. A cancelled context arrives here as an ordinary S2 failure,
// which is the intended landing place for the module deadline expiring during
// the final sample.
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
		// A relid absent from the join is a relation that vanished between the
		// two statements. Its sizes stay empty, which is the truth - and stays
		// distinct from a relation that has no storage of its own, which the
		// size functions report as 0 rather than NULL.
		if found, ok := sizes[rows[i].relid]; ok {
			rows[i].tableSize = found.table
			rows[i].indexSize = found.index
		}
	}

	return nil
}

// bloatCells renders the rows in bloatColumns order.
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
