package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultTopIndexes is how many indexes each of a sample's two ranked lists holds.
const DefaultTopIndexes = 50

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

// The $1 least-used indexes, largest first among ties, and the $1 most-used,
// ranked from one read so an index in both is one row. Sized here because the
// ranking needs it. indexrelid settles ties alike every sample; NULLS LAST keeps
// an index dropped mid-statement from ranking as the largest.
const indexUsageStatsSQL = `WITH indexes AS MATERIALIZED (
    SELECT indexrelid,
           relid,
           indexrelname::text AS indexrelname,
           idx_scan,
           idx_tup_read,
           idx_tup_fetch,
           pg_relation_size(indexrelid) AS index_size_bytes,
           count(*) OVER () AS indexes_total
    FROM pg_catalog.pg_stat_user_indexes
)
(SELECT * FROM indexes ORDER BY idx_scan, index_size_bytes DESC NULLS LAST, indexrelid LIMIT $1)
UNION
(SELECT * FROM indexes ORDER BY idx_scan DESC, indexrelid LIMIT $1)
ORDER BY indexrelid`

// IndexUsage captures the pg_stat_user_indexes rows the two ranked lists choose,
// every sample. Each sample ranks afresh, so two samples can hold different
// indexes. An index whose idx_scan never moves across the window is the finding
// this artifact exists for.
type IndexUsage struct {
	// Interval is the cadence, the run's expensive speed. Zero is the bookend alone.
	Interval time.Duration

	// Top is how many indexes each ranked list holds. Zero takes DefaultTopIndexes.
	Top int
}

func (u IndexUsage) Artifact() Artifact {
	return Artifact{
		Name:       "pg_index_usage",
		FileName:   "pg_index_usage.txt",
		Scope:      "database",
		Schedule:   Periodic(u.Interval),
		Connection: ConnectionExpensive,
		Version:    sampleHeaderVersion,

		// No SampleBudget: one statement is within DefaultSampleBudget, and that is
		// what this collector adds to the closing tick - Periodic's last sample is
		// the close, so moduleDeadline sums it there like the others.
	}
}

// Sample runs the one statement and writes one block. A failed statement errors
// and writes nothing.
func (u IndexUsage) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	start := s.now()

	rows, total, err := u.readStats(ctx, q)
	if err != nil {
		return err
	}

	reads := span{start, s.now()}

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the view read; the window's own blocks name the artifact. Never
	// truncated: the lists are the selection, and indexes_total= is what they
	// were chosen from.
	if err := writeSampleHeader(&block, u.Artifact(), s, sampleHeader{
		source: "pg_stat_user_indexes",
		reads:  reads,
		status: statusOK,
		rows:   len(rows),
		fields: []headerField{
			{"indexes_written", strconv.Itoa(len(rows))},
			{"indexes_total", strconv.FormatInt(total, 10)},
		},
	}); err != nil {
		return err
	}

	if err := writeRows(&block, indexUsageColumns, indexUsageCells(rows)); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// indexUsageRow is one index as read. The counters are pointers for the
// package's rule - empty means "not read" - not because the view returns NULL
// here: the pg_stat_get_* functions behind it report 0 for an index with no
// statistics entry yet, and 0 is the reading. An index never scanned must
// survive as 0. A NULL size is an index dropped mid-statement, which
// pg_relation_size reports as NULL where no storage is 0.
type indexUsageRow struct {
	indexrelid   uint32
	relid        uint32
	indexRelName string
	idxScan      *int64
	idxTupRead   *int64
	idxTupFetch  *int64
	indexSize    *int64
}

// readStats returns the ranked lists' rows and how many indexes they were chosen from.
func (u IndexUsage) readStats(ctx context.Context, q RowQuerier) ([]indexUsageRow, int64, error) {
	top := u.Top
	if top <= 0 {
		top = DefaultTopIndexes
	}

	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, indexUsageStatsSQL, top)
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
			&row.indexSize,
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
