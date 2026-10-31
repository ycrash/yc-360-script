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

// slowQueriesServer counts CPU time and has memory instruments on.
func slowQueriesServer(rows ...[]any) *fakeQuerier {
	return (&fakeQuerier{}).
		on("performance_schema.setup_consumers", fakeResult{columns: []string{"cpu", "memory"}, rows: [][]any{{3, 429}}}).
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

func TestSlowQueriesWritesNoMemoryWithTheMemoryInstrumentsOff(t *testing.T) {
	q := (&fakeQuerier{}).
		on("performance_schema.setup_consumers", fakeResult{columns: []string{"cpu", "memory"}, rows: [][]any{{3, 0}}}).
		on("events_statements_summary_by_digest", fakeResult{columns: slowQueryColumns})

	var out bytes.Buffer

	require.NoError(t, (&SlowQueries{Interval: time.Minute}).Sample(context.Background(), q, &out, testSample(1, 2)))

	statement := q.sent[len(q.sent)-1]
	assert.Contains(t, statement, "sum_cpu_time AS sum_cpu_time")
	assert.Contains(t, statement, "NULL AS max_total_memory, NULL AS max_controlled_memory",
		"the server reads zero for them, which is not a measurement")
	assert.Contains(t, out.String(), "unavailable=max_total_memory,max_controlled_memory ")
}

func TestSlowQueriesAsksARefusedConsumerCheckOnce(t *testing.T) {
	consumersDenied := serverError(errTableAccess, "SELECT command denied to user 'yc'@'%' for table 'setup_consumers'")
	digestsDenied := serverError(errTableAccess,
		"SELECT command denied to user 'yc'@'%' for table 'events_statements_summary_by_digest'")

	run := func(q *fakeQuerier) {
		t.Helper()

		sq := &SlowQueries{Interval: time.Minute}

		for i := 1; i <= 3; i++ {
			var out bytes.Buffer

			require.NoError(t, sq.Sample(context.Background(), q, &out, testSample(i, 3)))
		}
	}

	q := (&fakeQuerier{}).
		on("performance_schema.setup_consumers", fakeResult{err: consumersDenied}).
		on("events_statements_summary_by_digest", fakeResult{columns: slowQueryColumns})
	run(q)

	assert.Equal(t, 1, q.count("setup_consumers"), "each refusal counts once more in the server's error statistics")
	assert.Equal(t, 3, q.count("events_statements_summary_by_digest"), "the digests are still read")

	q = (&fakeQuerier{}).
		on("performance_schema.setup_consumers", fakeResult{columns: []string{"cpu", "memory"}, rows: [][]any{{3, 429}}}).
		on("events_statements_summary_by_digest", fakeResult{err: digestsDenied})
	run(q)

	assert.Equal(t, 1, q.count("setup_consumers"), "nothing to describe once the digests are refused")
	assert.Equal(t, 1, q.count("events_statements_summary_by_digest"))
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
	assert.Equal(t, 1, q.count("EXPLAIN FORMAT=JSON FOR SCHEMA `yc_app` SELECT * FROM orders"))
	assert.Zero(t, q.count("USE "), "the session's schema is never changed on 8.4")

	out.Reset()
	sq.read = 2

	require.NoError(t, explain.Sample(context.Background(), q, &out, testSample(2, 2)))
	assert.Equal(t, 1, q.count("EXPLAIN FORMAT=JSON"), "a digest is explained once per capture")
	assert.Contains(t, out.String(), "reason=no_new_digest plans=1")
}

func TestExplainWithoutForSchemaLeavesThePlansSchema(t *testing.T) {
	for _, c := range []struct {
		name     string
		facts    Facts
		database string
		restore  string
	}{
		{"8.0, no database", Facts{Version: Version{8, 0, 46}, Variant: VariantMySQL}, "", "USE `information_schema`"},
		{"8.0, a database", Facts{Version: Version{8, 0, 46}, Variant: VariantMySQL}, "orders_db", "USE `orders_db`"},
		{"MariaDB", Facts{Version: Version{10, 11, 6}, Variant: VariantMariaDB}, "", "USE `information_schema`"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sq := &SlowQueries{Interval: time.Minute}
			sq.latest = []explainCandidate{{schema: "yc_app", digest: "d1", text: "SELECT * FROM orders WHERE id = 1"}}
			sq.read = 1

			q := (&fakeQuerier{}).
				on("@@performance_schema_max_sql_text_length", fakeResult{columns: []string{"v"}, rows: [][]any{{1024}}}).
				on("USE ", fakeResult{}).
				on("EXPLAIN FORMAT=JSON", fakeResult{columns: []string{"EXPLAIN"}, rows: [][]any{{testPlan}}})

			sample := testSample(1, 2)
			sample.Engine = c.facts

			var out bytes.Buffer

			require.NoError(t, NewExplain("all", sq, time.Minute, c.database).Sample(context.Background(), q, &out, sample))

			assert.Equal(t, []string{
				"@@performance_schema_max_sql_text_length", "USE `yc_app`", "EXPLAIN FORMAT=JSON SELECT", c.restore,
			}, sentKeys(q.sent, "@@performance_schema_max_sql_text_length", "USE `yc_app`", "EXPLAIN FORMAT=JSON SELECT",
				c.restore), "the plan's schema, then one that is not the application's")
		})
	}
}

// sentKeys is, for each statement sent, the first key it contains, in order.
func sentKeys(sent []string, keys ...string) []string {
	var found []string

	for _, statement := range sent {
		for _, key := range keys {
			if strings.Contains(statement, key) {
				found = append(found, key)

				break
			}
		}
	}

	return found
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

// planV2 is 9.x's default JSON plan format, as my9 returned it: the rewritten query
// and each operation carry the statement's values.
const planV2 = `{
  "query": "/* select#1 */ select ` + "`yc_app`.`orders`.`id` AS `id`" + ` from ` + "`yc_app`.`orders`" + ` where ((` + "`yc_app`.`orders`.`customer_id`" + ` = 7) and (` + "`yc_app`.`orders`.`status`" + ` = 'pending'))",
  "query_plan": {
    "inputs": [
      {
        "operation": "Index lookup on orders using orders_customer_idx (customer_id = 7)",
        "index_name": "orders_customer_idx",
        "table_name": "orders",
        "access_type": "index",
        "key_columns": [
          "customer_id"
        ],
        "estimated_rows": 10.0,
        "lookup_condition": "customer_id = 7",
        "lookup_references": [
          "const"
        ]
      }
    ],
    "condition": "(orders.` + "`status`" + ` = 'pending')",
    "operation": "Filter: (orders.` + "`status`" + ` = 'pending')",
    "access_type": "filter"
  },
  "query_type": "select",
  "json_schema_version": "2.0"
}`

func TestRedactPlanReplacesValuesWhereverTheFormatPutsThem(t *testing.T) {
	got, n := redactPlan(planV2)

	assert.NotContains(t, got, "'pending'")
	assert.NotContains(t, got, "= 7")
	assert.Contains(t, got, `"operation": "Index lookup on orders using orders_customer_idx (customer_id = <redacted>)"`)
	assert.Contains(t, got, `"lookup_condition": "customer_id = <redacted>"`)
	assert.Contains(t, got, `"operation": "Filter: (orders.`+"`status`"+` = '<redacted>')"`)
	assert.Contains(t, got, "= '<redacted>'))\",\n  \"query_plan\"", "the rewritten query keeps its shape")
	assert.Contains(t, got, `"index_name": "orders_customer_idx",`)
	assert.Contains(t, got, "\"key_columns\": [\n          \"customer_id\"\n        ],", "names, and the layout, are kept")
	assert.Contains(t, got, `"json_schema_version": "2.0"`)
	assert.Contains(t, got, `"estimated_rows": 10.0,`)
	assert.Equal(t, 6, n)

	unchanged := `{"query_block": {"select_id": 1, "message": "No tables used"}}`
	got, n = redactPlan(unchanged)
	assert.Equal(t, unchanged, got)
	assert.Zero(t, n)
}

func TestExplainSkipsAPreparedStatementsSample(t *testing.T) {
	assert.True(t, hasPlaceholder("SELECT id FROM t WHERE status = 'x' AND customer_id = ?"))
	assert.False(t, hasPlaceholder("SELECT 'what?', `odd?name` FROM t /* why? */ WHERE a = 1 # really?"))
	assert.False(t, hasPlaceholder("SELECT _utf8mb4'?' FROM t"))
}
