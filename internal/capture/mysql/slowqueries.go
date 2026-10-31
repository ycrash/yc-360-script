package mysql

import (
	"context"
	"io"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// slowQueryColumns are the spec's, in its order. A column this server has no source
// for is still written, empty, and named on the block as unavailable=: a reader sees
// one shape for every server.
var slowQueryColumns = []string{
	"schema_name", "digest", "digest_text", "query_sample_text", "query_sample_seen", "query_sample_timer_wait",
	"count_star", "sum_timer_wait", "sum_rows_examined", "sum_rows_sent",
	"sum_cpu_time", "sum_lock_time",
	"quantile_95", "quantile_99", "max_timer_wait", "sum_errors",
	"max_total_memory", "max_controlled_memory",
	"sum_created_tmp_disk_tables",
}

// countingSQL reads, in one statement, the switches two of the digest columns need.
// sum_cpu_time needs its consumer and the two every consumer below them needs.
// max_total_memory and max_controlled_memory need the memory instruments, and with
// them off read zero, not NULL (measured on my80). Checked, never switched on.
const countingSQL = `SELECT
  (SELECT COUNT(*) FROM performance_schema.setup_consumers
    WHERE NAME IN ('events_statements_cpu', 'global_instrumentation', 'thread_instrumentation') AND ENABLED = 'YES'),
  (` + memoryInstrumentsSQL + ` AND ENABLED = 'YES')`

// The reads the collector's blockState remembers a refusal of: the digest block,
// and the counting check, which is no block but counts against the server's error
// statistics all the same.
const (
	digestSource   = "events_statements_summary_by_digest"
	countingSource = "setup_consumers"
)

// explainCandidate is a digest's sample statement as the server holds it, values
// and all: what EXPLAIN is run against, and never written.
type explainCandidate struct {
	schema string
	digest string
	text   string
}

// SlowQueries is my_slow_queries.txt: the 100 digests with the most cumulative time,
// of rows_total=, keyed by (schema_name, digest) - the table's own key, so two
// schemas running the same statement never merge. query_sample_text is written with its values
// replaced; the text as read is kept in memory for my_explain.txt, which reads this
// collector's latest read rather than running the statement again.
type SlowQueries struct {
	Interval time.Duration

	state blockState

	// latest is the last sample's candidates, in the order read.
	latest []explainCandidate
	read   int
}

func (sq *SlowQueries) Artifact() Artifact {
	return periodicArtifact("my_slow_queries", ConnectionNormal, sq.Interval)
}

func (sq *SlowQueries) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	version := s.Engine.Version

	// Nothing to ask before 8.0.28, which has none of the columns a switch decides;
	// an unread version reads them as absent rather than fail the whole statement on
	// an older server. Nothing either with performance_schema off, whose digests are
	// not read.
	var counting digestCounting
	if version.AtLeast(8, 0, 28) && !s.Engine.PerformanceSchemaOff {
		counting = sq.counting(ctx, q, s)
	}

	var unavailable []string

	// sum_cpu_time is 8.0.28. Zero while the consumer is off would read as "no CPU":
	// it is not measured.
	cpuTime := "sum_cpu_time"
	if !counting.cpuTime {
		cpuTime = "NULL"

		unavailable = append(unavailable, "sum_cpu_time")
	}

	// max_total_memory and max_controlled_memory are 8.0.31, and not measured with
	// the memory instruments off either.
	totalMemory, controlledMemory := "max_total_memory", "max_controlled_memory"
	if !version.AtLeast(8, 0, 31) || !counting.memory {
		totalMemory, controlledMemory = "NULL", "NULL"

		unavailable = append(unavailable, "max_total_memory", "max_controlled_memory")
	}

	// query_sample_seen is read as text. A digest with no sample holds the zero
	// datetime there, and returning that as a datetime raises warning 1292 once per
	// such row, on every sample, into the error statistics my_errors.txt reads. As
	// text it is the same bytes, and raises nothing.
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

	// Read raw for my_explain.txt, then written through the same path as every
	// other block, which replaces the values.
	return writeBlocks(ctx, &retainingQuerier{Querier: q, sq: sq, s: s}, w, sq.Artifact(), s, &sq.state, []block{b})
}

// digestCounting is what countingSQL found counted; nothing, where it was not read.
type digestCounting struct {
	cpuTime bool
	memory  bool
}

// counting reads what the switched columns are counting; a failed read counts as
// nothing, so a zero is never written for either. A refusal is asked once and
// remembered, as a block's is, and nothing is asked once the digest read has been
// refused: there is no row for the answer to describe.
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

// Latest is the candidates of the last sample read, with the sample's index.
func (sq *SlowQueries) Latest() ([]explainCandidate, int) { return sq.latest, sq.read }

// retainingQuerier hands the slow-queries statement's rows to the collector as they
// are read, values and all, before the block writer replaces them.
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

// retainingRows copies schema_name, digest and query_sample_text from each row the
// block writer scans.
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
