package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultMaxIndexes bounds one sample against a pathological schema. Twice
// DefaultMaxTables: indexes outnumber tables, and a schema at bloat's cap with
// two indexes per table still fits under this one.
const DefaultMaxIndexes = 2 * DefaultMaxTables

// indexrelid leads: it's the join key across samples. relid is the table's, the
// key into pg_bloat.txt, which is where the schema and table names live.
var indexUsageColumns = []string{
	"indexrelid",
	"relid",
	"indexrelname",
	"idx_scan",
	"idx_tup_read",
	"idx_tup_fetch",
	"index_size_bytes",
}

// Two statements, the shape pg_bloat.txt settled on rather than one statement:
// the size function stats every index's files on a filesystem that may itself
// be sick, and as one statement a large schema could exceed statement_timeout
// and lose the scan counts with it.

// ORDER BY indexrelid for determinism only: ordering on a statistic would change
// between samples and cap on two different index sets.
const indexUsageStatsSQL = `SELECT indexrelid,
       relid,
       indexrelname::text,
       idx_scan,
       idx_tup_read,
       idx_tup_fetch,
       count(*) OVER () AS indexes_total
FROM pg_catalog.pg_stat_user_indexes
ORDER BY indexrelid
LIMIT $1`

// Queries S1's indexrelids directly, not a second view scan that could return a
// different row set. LEFT JOIN: an index dropped between statements yields a
// NULL size rather than a vanished OID reaching pg_relation_size.
const indexUsageSizesSQL = `SELECT o AS indexrelid,
       pg_relation_size(c.oid) AS index_size_bytes
FROM unnest($1::oid[]) AS o
LEFT JOIN pg_catalog.pg_class c ON c.oid = o`

// IndexUsage captures pg_stat_user_indexes every sample; deltas are computed
// downstream by joining consecutive blocks on indexrelid. An index whose
// idx_scan never moves across the window is the finding this artifact exists for.
type IndexUsage struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// MaxIndexes bounds one sample. Zero takes DefaultMaxIndexes.
	MaxIndexes int
}

func (u IndexUsage) Artifact() Artifact {
	return Artifact{
		Name:     "pg_index_usage",
		FileName: "pg_index_usage.txt",
		Scope:    "database",
		Schedule: Periodic(u.Interval),

		// No SampleBudget: two statements is DefaultSampleBudget already, and that
		// is what this collector adds to the closing tick - Periodic's last sample
		// is the close, so moduleDeadline sums it there like the others.
	}
}

// Sample runs S1, then S2 over S1's indexrelids, and writes one block. A failed
// S1 errors and writes nothing; a failed S2 leaves the size column empty but
// still writes the sample.
func (u IndexUsage) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, total, err := u.readStats(ctx, q)
	if err != nil {
		return err
	}

	sizesErr := readIndexSizes(ctx, q, rows)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
		{"indexes_written", strconv.Itoa(len(rows))},
		{"indexes_total", strconv.FormatInt(total, 10)},
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
	if err := writeBlockHeader(&block, "pg_stat_user_indexes", u.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	if err := writeRows(&block, indexUsageColumns, indexUsageCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// indexUsageRow holds one index between the two statements so its size can be
// merged on by indexrelid. The counters are pointers for the package's rule -
// empty means "not read" - not because the view returns NULL here: the
// pg_stat_get_* functions behind it report 0 for an index with no statistics
// entry yet, and 0 is the reading. An index never scanned must survive as 0.
type indexUsageRow struct {
	indexrelid   uint32
	relid        uint32
	indexRelName string
	idxScan      *int64
	idxTupRead   *int64
	idxTupFetch  *int64

	indexSize *int64
}

// readStats returns the capped rows and the uncapped total.
func (u IndexUsage) readStats(ctx context.Context, q RowQuerier) ([]indexUsageRow, int64, error) {
	limit := u.MaxIndexes
	if limit <= 0 {
		limit = DefaultMaxIndexes
	}

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, indexUsageStatsSQL, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []indexUsageRow
		total     int64
	)

	for rows.Next() {
		var row indexUsageRow

		if err := rows.Scan(
			&row.indexrelid,
			&row.relid,
			&row.indexRelName,
			&row.idxScan,
			&row.idxTupRead,
			&row.idxTupFetch,
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

// readIndexSizes merges S2 onto rows in place. Nothing is assigned until the
// whole result is read, so a failure mid-read leaves every row's size empty
// rather than some sized and some not.
func readIndexSizes(ctx context.Context, q RowQuerier, rows []indexUsageRow) error {
	if len(rows) == 0 {
		return nil
	}

	indexrelids := make([]uint32, len(rows))
	for i := range rows {
		indexrelids[i] = rows[i].indexrelid
	}

	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	sized, err := q.Query(stmtCtx, indexUsageSizesSQL, indexrelids)
	if err != nil {
		return err
	}
	defer sized.Close()

	sizes := make(map[uint32]*int64, len(rows))

	for sized.Next() {
		var (
			indexrelid uint32
			size       *int64
		)

		if err := sized.Scan(&indexrelid, &size); err != nil {
			return err
		}

		sizes[indexrelid] = size
	}

	if err := sized.Err(); err != nil {
		return err
	}

	for i := range rows {
		// An indexrelid absent vanished mid-sample: its size stays empty, distinct
		// from "no storage", which pg_relation_size reports as 0, not NULL.
		if size, ok := sizes[rows[i].indexrelid]; ok {
			rows[i].indexSize = size
		}
	}

	return nil
}

func indexUsageCells(rows []indexUsageRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			strconv.FormatUint(uint64(row.indexrelid), 10),
			strconv.FormatUint(uint64(row.relid), 10),
			row.indexRelName,
			int64Text(row.idxScan),
			int64Text(row.idxTupRead),
			int64Text(row.idxTupFetch),
			int64Text(row.indexSize),
		}
	}

	return cells
}
