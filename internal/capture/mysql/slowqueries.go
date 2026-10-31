package mysql

import (
	"context"
	"io"
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
	"max_total_memory", "max_controlled_memory",
	"sum_created_tmp_disk_tables",
}

// cpuConsumerSQL reads whether sum_cpu_time is being counted: its consumer, and the
// two every consumer below them needs. Checked, never switched on.
const cpuConsumerSQL = `SELECT COUNT(*) FROM performance_schema.setup_consumers
WHERE NAME IN ('events_statements_cpu', 'global_instrumentation', 'thread_instrumentation') AND ENABLED = 'YES'`

// The reads the collector's blockState remembers a refusal of: the digest block,
// and the consumer check, which is no block but counts against the server's error
// statistics all the same.
const (
	digestSource   = "events_statements_summary_by_digest"
	consumerSource = "setup_consumers"
)

// explainCandidate is a digest's sample statement as the server holds it, values
// and all: what EXPLAIN is run against, and never written.
type explainCandidate struct {
	schema string
	digest string
	text   string
}

// SlowQueries is my_slow_queries.txt: the 100 digests with the most cumulative time,
// keyed by (schema_name, digest) - the table's own key, so two schemas running the
// same statement never merge. query_sample_text is written with its values
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

	var unavailable []string

	cpuTime := "sum_cpu_time"

	switch {
	// sum_cpu_time is 8.0.28; an unread version reads it as absent rather than fail
	// the whole statement on an older server.
	case !version.AtLeast(8, 0, 28):
		cpuTime = "NULL"

		unavailable = append(unavailable, "sum_cpu_time")

	case !sq.cpuCounted(ctx, q, s):
		// Zero while the consumer is off would read as "no CPU": it is not measured.
		cpuTime = "NULL"

		unavailable = append(unavailable, "sum_cpu_time")
	}

	totalMemory, controlledMemory := "max_total_memory", "max_controlled_memory"
	if !version.AtLeast(8, 0, 31) {
		totalMemory, controlledMemory = "NULL", "NULL"

		unavailable = append(unavailable, "max_total_memory", "max_controlled_memory")
	}

	query := `SELECT schema_name, digest, digest_text, query_sample_text, query_sample_seen,
       query_sample_timer_wait, count_star, sum_timer_wait, sum_rows_examined, sum_rows_sent,
       ` + cpuTime + ` AS sum_cpu_time, sum_lock_time,
       ` + totalMemory + ` AS max_total_memory, ` + controlledMemory + ` AS max_controlled_memory,
       sum_created_tmp_disk_tables
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
	}

	// Read raw for my_explain.txt, then written through the same path as every
	// other block, which replaces the values.
	return writeBlocks(ctx, &retainingQuerier{Querier: q, sq: sq, s: s}, w, sq.Artifact(), s, &sq.state, []block{b})
}

// cpuCounted reports whether sum_cpu_time is being counted; a failed read counts as
// not, so a zero is never written for it. A refusal is asked once and remembered,
// as a block's is, and nothing is asked once the digest read has been refused:
// there is no row for the answer to describe.
func (sq *SlowQueries) cpuCounted(ctx context.Context, q Querier, s SampleContext) bool {
	if sq.state.isRefused(consumerSource) || sq.state.isRefused(digestSource) {
		return false
	}

	row, err := readRow(ctx, q, s, cpuConsumerSQL)
	if err != nil && permissionDenied(err) {
		sq.state.refuse(consumerSource)
	}

	return err == nil && len(row) == 1 && row[0] == "3"
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
