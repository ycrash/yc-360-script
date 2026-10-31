package mysql

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// slowQueryColumns are the spec's, in order. A column the server lacks is written
// empty and named in unavailable=, so every server has one shape.
var slowQueryColumns = []string{
	"schema_name", "digest", "digest_text", "query_sample_text", "query_sample_seen", "query_sample_timer_wait",
	"count_star", "sum_timer_wait", "sum_rows_examined", "sum_rows_sent",
	"sum_cpu_time", "sum_lock_time",
	"quantile_95", "quantile_99", "max_timer_wait", "sum_errors",
	"max_total_memory", "max_controlled_memory",
	"sum_created_tmp_disk_tables",
}

// countingSQL checks (never enables) what sum_cpu_time and the max_*_memory columns
// need: three consumers, and the memory instruments, without which they read zero,
// not NULL (measured on my80).
const countingSQL = `SELECT
  (SELECT COUNT(*) FROM performance_schema.setup_consumers
    WHERE NAME IN ('events_statements_cpu', 'global_instrumentation', 'thread_instrumentation') AND ENABLED = 'YES'),
  (` + memoryInstrumentsSQL + ` AND ENABLED = 'YES')`

// Sources whose refusal blockState remembers.
const (
	digestSource   = "events_statements_summary_by_digest"
	countingSource = "setup_consumers"
)

// explainCandidate is a digest's unredacted sample statement, for EXPLAIN; never written.
type explainCandidate struct {
	schema string
	digest string
	text   string
}

// SlowQueries is my_slow_queries.txt: the top 100 digests by total time, keyed by
// (schema_name, digest). Unredacted samples are kept in memory for my_explain.txt.
type SlowQueries struct {
	Interval time.Duration

	state blockState

	latest []explainCandidate
	read   int
}

func (sq *SlowQueries) Artifact() Artifact {
	return periodicArtifact("my_slow_queries", ConnectionNormal, sq.Interval)
}

func (sq *SlowQueries) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	version := s.Engine.Version

	// The switched columns are 8.0.28+; an unread version treats them as absent.
	var counting digestCounting
	if version.AtLeast(8, 0, 28) && !s.Engine.PerformanceSchemaOff {
		counting = sq.counting(ctx, q, s)
	}

	var unavailable []string

	// NULL, not zero, while the consumer is off: zero would read as "no CPU".
	cpuTime := "sum_cpu_time"
	if !counting.cpuTime {
		cpuTime = "NULL"

		unavailable = append(unavailable, "sum_cpu_time")
	}

	// 8.0.31+, and need the memory instruments.
	totalMemory, controlledMemory := "max_total_memory", "max_controlled_memory"
	if !version.AtLeast(8, 0, 31) || !counting.memory {
		totalMemory, controlledMemory = "NULL", "NULL"

		unavailable = append(unavailable, "max_total_memory", "max_controlled_memory")
	}

	// query_sample_seen as text: a zero datetime would raise warning 1292 per row,
	// polluting my_errors.txt.
	query := `SELECT schema_name, digest, digest_text, query_sample_text,
       CAST(query_sample_seen AS CHAR) AS query_sample_seen,
       query_sample_timer_wait, count_star, sum_timer_wait, sum_rows_examined, sum_rows_sent,
       ` + cpuTime + ` AS sum_cpu_time, sum_lock_time,
       quantile_95, quantile_99, max_timer_wait, sum_errors,
       ` + totalMemory + ` AS max_total_memory, ` + controlledMemory + ` AS max_controlled_memory,
       sum_created_tmp_disk_tables,
       ` + rowsTotalSelect + `
FROM performance_schema.events_statements_summary_by_digest
ORDER BY sum_timer_wait DESC
LIMIT 100`

	var fields []dbcore.HeaderField
	if len(unavailable) > 0 {
		fields = append(fields, kv("unavailable", strings.Join(unavailable, ",")))
	}

	b := block{
		source:           digestSource,
		query:            query,
		columns:          slowQueryColumns,
		statementColumns: []string{"query_sample_text"},
		fields:           fields,
		counted:          true,
		instrumented:     true,
	}

	// Rows are retained raw for my_explain.txt, then written redacted.
	return writeBlocks(ctx, &retainingQuerier{Querier: q, sq: sq, s: s}, w, sq.Artifact(), s, &sq.state, []block{b})
}

// digestCounting is what countingSQL found enabled.
type digestCounting struct {
	cpuTime bool
	memory  bool
}

// counting runs countingSQL; failure means nothing is counted. Skipped once it or
// the digest read has been refused.
func (sq *SlowQueries) counting(ctx context.Context, q Querier, s SampleContext) digestCounting {
	if sq.state.isRefused(countingSource) || sq.state.isRefused(digestSource) {
		return digestCounting{}
	}

	row, err := readRow(ctx, q, s, countingSQL)

	switch {
	case err != nil && permissionDenied(err):
		sq.state.refuse(countingSource)

		return digestCounting{}

	case err != nil || len(row) != 2:
		return digestCounting{}
	}

	memory, _ := strconv.ParseInt(row[1], 10, 64)

	return digestCounting{cpuTime: row[0] == "3", memory: memory > 0}
}

// Latest returns the last read's candidates and its sample index.
func (sq *SlowQueries) Latest() ([]explainCandidate, int) { return sq.latest, sq.read }

// retainingQuerier captures rows unredacted before the block writer redacts them.
type retainingQuerier struct {
	Querier

	sq *SlowQueries
	s  SampleContext
}

func (r *retainingQuerier) Query(ctx context.Context, query string) (Rows, error) {
	rows, err := r.Querier.Query(ctx, query)
	if err != nil {
		return nil, err
	}

	r.sq.latest = nil
	r.sq.read = r.s.Index

	return &retainingRows{Rows: rows, sq: r.sq}, nil
}

// retainingRows copies schema_name, digest and query_sample_text from each scanned row.
type retainingRows struct {
	Rows

	sq *SlowQueries
}

func (r *retainingRows) Scan(dest ...any) error {
	if err := r.Rows.Scan(dest...); err != nil {
		return err
	}

	values := scannedStrings(dest)
	if len(values) >= 4 {
		r.sq.latest = append(r.sq.latest, explainCandidate{schema: values[0], digest: values[1], text: values[3]})
	}

	return nil
}
