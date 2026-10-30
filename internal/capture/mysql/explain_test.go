package mysql

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func digestRow(schema, digest, sample string) []any {
	return []any{schema, digest, "SELECT ...", sample, "2026-10-27 09:00:00", 1, 10, 100, 5, 5, nil, 0, 1024, 512, 0}
}

func slowQueriesServer(rows ...[]any) *fakeQuerier {
	return (&fakeQuerier{}).
		on("performance_schema.setup_consumers", fakeResult{columns: []string{"n"}, rows: [][]any{{3}}}).
		on("events_statements_summary_by_digest", fakeResult{columns: slowQueryColumns, rows: rows})
}

func TestSlowQueriesReadsWhatThisVersionHas(t *testing.T) {
	old := testSample(1, 2)
	old.Engine.Version = Version{8, 0, 27}

	q := slowQueriesServer()

	var out bytes.Buffer

	require.NoError(t, (&SlowQueries{Interval: time.Minute}).Sample(context.Background(), q, &out, old))

	statement := q.sent[len(q.sent)-1]
	assert.Contains(t, statement, "NULL AS sum_cpu_time")
	assert.Contains(t, statement, "NULL AS max_total_memory")
	assert.Zero(t, q.count("setup_consumers"), "no consumer to ask about before 8.0.28")
	assert.Contains(t, out.String(), "unavailable=sum_cpu_time,max_total_memory,max_controlled_memory")

	q = slowQueriesServer()
	out.Reset()

	require.NoError(t, (&SlowQueries{Interval: time.Minute}).Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Contains(t, q.sent[len(q.sent)-1], "sum_cpu_time AS sum_cpu_time")
	assert.NotContains(t, out.String(), "unavailable=")
}

func TestSlowQueriesWritesTheSampleRedactedAndKeepsItForExplain(t *testing.T) {
	q := slowQueriesServer(digestRow("yc_app", "d1", "SELECT * FROM orders WHERE customer_id = 7"))

	sq := &SlowQueries{Interval: time.Minute}

	var out bytes.Buffer

	require.NoError(t, sq.Sample(context.Background(), q, &out, testSample(1, 2)))

	assert.Contains(t, out.String(), "SELECT * FROM orders WHERE customer_id = <redacted>")
	assert.NotContains(t, out.String(), "= 7")

	candidates, read := sq.Latest()
	require.Len(t, candidates, 1)
	assert.Equal(t, 1, read)
	assert.Equal(t, explainCandidate{schema: "yc_app", digest: "d1", text: "SELECT * FROM orders WHERE customer_id = 7"},
		candidates[0], "EXPLAIN needs the runnable text, values and all")
}

const testPlan = `{
  "query_block": {
    "select_id": 1,
    "cost_info": {"query_cost": "51.25"},
    "table": {
      "table_name": "orders",
      "access_type": "ref",
      "key": "orders_customer_idx",
      "rows_examined_per_scan": 10,
      "attached_condition": "((` + "`yc_app`.`orders`.`status`" + ` = 'pending') and (` + "`yc_app`.`orders`.`id`" + ` > 100))"
    }
  }
}`

func TestExplainCapturesOnePlanPerDigestAtFirstSight(t *testing.T) {
	sq := &SlowQueries{Interval: time.Minute}
	sq.latest = []explainCandidate{
		{schema: "yc_app", digest: "d1", text: "SELECT * FROM orders WHERE status = 'pending' AND id > 100"},
		{schema: "yc_app", digest: "d2", text: "/* yCrash-DB-Agent */ SELECT 1"},
		{schema: "yc_app", digest: "d3", text: "UPDATE orders SET status = 'x'"},
		{schema: "", digest: "d4", text: ""},
		{schema: "yc_app", digest: "d5", text: "SHOW TABLES"},
	}
	sq.read = 1

	q := (&fakeQuerier{}).
		on("@@performance_schema_max_sql_text_length", fakeResult{columns: []string{"v"}, rows: [][]any{{1024}}}).
		on("USE `yc_app`", fakeResult{}).
		on("USE `orders_db`", fakeResult{}).
		on("EXPLAIN FORMAT=JSON", fakeResult{columns: []string{"EXPLAIN"}, rows: [][]any{{testPlan}}})

	explain := NewExplain("all", sq, time.Minute, "orders_db")

	var out bytes.Buffer

	require.NoError(t, explain.Sample(context.Background(), q, &out, testSample(1, 2)))

	text := out.String()
	assert.Contains(t, text, "digest=d1 mode=ESTIMATED redacted=2")
	assert.Contains(t, text, `"attached_condition": "((`+"`yc_app`.`orders`.`status`"+` = '<redacted>') and (`+
		"`yc_app`.`orders`.`id`"+` > <redacted>))"`)
	assert.Contains(t, text, `"query_cost": "51.25"`, "costs and estimates are kept")
	assert.Contains(t, text, "digest=d2 mode=ESTIMATED reason=agent_statement")
	assert.Contains(t, text, "digest=d3 mode=ESTIMATED reason=read_only_session")
	assert.Contains(t, text, "digest=d4 mode=ESTIMATED reason=no_sample_text")
	assert.Contains(t, text, "digest=d5 mode=ESTIMATED reason=not_explainable")
	assert.Equal(t, 1, q.count("EXPLAIN FORMAT=JSON"))
	assert.Equal(t, 1, q.count("USE `orders_db`"), "the configured default schema is put back")

	out.Reset()
	sq.read = 2

	require.NoError(t, explain.Sample(context.Background(), q, &out, testSample(2, 2)))
	assert.Equal(t, 1, q.count("EXPLAIN FORMAT=JSON"), "a digest is explained once per capture")
	assert.Contains(t, out.String(), "reason=no_new_digest plans=1")
}

func TestExplainStopsAtFivePlans(t *testing.T) {
	sq := &SlowQueries{Interval: time.Minute, read: 1}
	for i := range 8 {
		sq.latest = append(sq.latest, explainCandidate{digest: strings.Repeat("d", i+1), text: "SELECT 1"})
	}

	q := (&fakeQuerier{}).
		on("@@performance_schema_max_sql_text_length", fakeResult{columns: []string{"v"}, rows: [][]any{{1024}}}).
		on("EXPLAIN FORMAT=JSON", fakeResult{columns: []string{"EXPLAIN"}, rows: [][]any{{"{}"}}})

	var out bytes.Buffer

	require.NoError(t, NewExplain("all", sq, time.Minute, "").Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Equal(t, MaxPlansPerCapture, q.count("EXPLAIN FORMAT=JSON"))
}

func TestExplainSkipsASampleTheServerCut(t *testing.T) {
	sq := &SlowQueries{Interval: time.Minute, read: 1,
		latest: []explainCandidate{{digest: "d1", text: "SELECT '" + strings.Repeat("x", 1020) + "'"}}}

	q := (&fakeQuerier{}).
		on("@@performance_schema_max_sql_text_length", fakeResult{columns: []string{"v"}, rows: [][]any{{1024}}})

	var out bytes.Buffer

	require.NoError(t, NewExplain("all", sq, time.Minute, "").Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Contains(t, out.String(), "reason=sample_text_cut")
	assert.Zero(t, q.count("EXPLAIN"))
}

func TestExplainOffSendsNothing(t *testing.T) {
	q := &fakeQuerier{}

	var out bytes.Buffer

	require.NoError(t, NewExplain("", &SlowQueries{}, time.Minute, "").Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Empty(t, q.sent)
	assert.Contains(t, out.String(), "reason=explain_off")
}
