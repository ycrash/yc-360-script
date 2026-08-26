package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- fixtures ----------------------------------------------------------------

const (
	explainSelfOID    = "16385"
	explainSearchPath = `"$user", public`

	// explainOwnerOID owns the statement fixtures; the activity join compares it.
	explainOwnerOID = "10"

	// explainMaxCharBytes is UTF-8's widest character, what pg_encoding_max_length returns.
	explainMaxCharBytes = int64(4)
)

// fakeExplainConn answers the log tail's resolution, the one activity read, the
// SET/RESET pair and each submitted EXPLAIN.
type fakeExplainConn struct {
	*fakeLogQuerier

	activity     []fakeResult
	activityArgs [][]any

	// plans is keyed by a substring of the candidate text, so submission order is free.
	plans map[string]fakeResult

	// defaultPlan answers any statement plans has no entry for.
	defaultPlan fakeResult

	submitted []string

	// protocols records how each statement was sent: the two modes must not swap.
	protocols []string

	utility []string
}

// protocolExtended and protocolSimple are what the fake records per statement.
const (
	protocolExtended = "extended"
	protocolSimple   = "simple"
)

// ExecSimple is the raw simple query protocol, which only the generic mode uses. Rows
// past maxBytes are drained and dropped and the cut reported, as the real one does.
func (c *fakeExplainConn) ExecSimple(ctx context.Context, sql string, maxBytes int) (
	[]string, bool, error,
) {
	c.submitted = append(c.submitted, sql)
	c.protocols = append(c.protocols, protocolSimple)

	result := c.planFor(sql)
	if result.err != nil {
		return nil, false, result.err
	}

	var (
		lines     []string
		held      int
		truncated bool
	)

	for _, row := range result.rows {
		line := row[0].(string)

		if maxBytes > 0 && held >= maxBytes {
			truncated = true

			continue
		}

		held += len(line) + 1

		lines = append(lines, line)
	}

	return lines, truncated, nil
}

func (c *fakeExplainConn) planFor(sql string) fakeResult {
	for text, result := range c.plans {
		if strings.Contains(sql, text) {
			return result
		}
	}

	return c.defaultPlan
}

func newFakeExplainConn(q *fakeLogQuerier) *fakeExplainConn {
	return &fakeExplainConn{
		fakeLogQuerier: q,
		activity:       repeat(rowsResult(activityValues())),
		plans:          map[string]fakeResult{},
		defaultPlan: rowsResult(planRows(
			"Seq Scan on public.orders  (cost=0.00..8420.00 rows=3 width=64)")),
	}
}

func (c *fakeExplainConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch {
	case sql == activitySQL:
		c.activityArgs = append(c.activityArgs, args)

		return answer(&c.activity)

	case sql == setExplainTimeoutSQL || sql == resetExplainTimeoutSQL:
		c.utility = append(c.utility, sql)

		return &fakeRows{}, nil

	case strings.HasPrefix(sql, "EXPLAIN "):
		c.submitted = append(c.submitted, sql)

		for _, arg := range args {
			if mode, ok := arg.(pgx.QueryExecMode); ok && mode == pgx.QueryExecModeExec {
				c.protocols = append(c.protocols, protocolExtended)
			}
		}

		return answerOne(c.planFor(sql))
	}

	return c.fakeLogQuerier.Query(ctx, sql, args...)
}

func answerOne(result fakeResult) (pgx.Rows, error) {
	if result.err != nil {
		return nil, result.err
	}

	return &fakeRows{values: result.rows}, nil
}

func planRows(lines ...string) [][]any {
	values := make([][]any, len(lines))
	for i, line := range lines {
		values[i] = []any{line}
	}

	return values
}

type activityFixture struct {
	pid        int32
	queryID    *int64
	usesysid   string
	state      string
	query      string
	queryBytes int64
	runningFor float64
	ranFor     float64
}

// activityValue builds one row of activitySQL's result, facts included.
func activityValue(f activityFixture) []any {
	queryStart := testWindowStart
	stateChange := queryStart.Add(time.Second)

	bytesRead := f.queryBytes
	if bytesRead == 0 {
		bytesRead = int64(len(f.query))
	}

	usesysid := f.usesysid
	if usesysid == "" {
		usesysid = explainOwnerOID
	}

	return []any{
		ptr(explainSearchPath), ptr(explainSelfOID), ptr(true), ptr(int64(1024)),
		ptr(explainMaxCharBytes),
		ptr(f.pid), f.queryID, ptr("16401"), ptr(usesysid), ptr(f.state),
		ptr(f.query), ptr(bytesRead), ptr(queryStart), ptr(stateChange),
		ptr(f.runningFor), ptr(f.ranFor),
	}
}

// activityValues is the default read: one idle session holding the top candidate's
// literal text.
func activityValues() [][]any {
	return [][]any{activityValue(activityFixture{
		pid:     4021,
		queryID: ptr(ordersItemsEnd.queryid),
		state:   "idle",
		query:   "SELECT * FROM order_items WHERE order_id = 4021",
		ranFor:  4.2,
	})}
}

// factsOnlyActivity is the LEFT JOIN's no-session shape: facts present, every
// activity column NULL.
func factsOnlyActivity() [][]any {
	return [][]any{{
		ptr(explainSearchPath), ptr(explainSelfOID), ptr(true), ptr(int64(1024)),
		ptr(explainMaxCharBytes),
		(*int32)(nil), (*int64)(nil), (*string)(nil), (*string)(nil), (*string)(nil),
		(*string)(nil), (*int64)(nil), (*time.Time)(nil), (*time.Time)(nil),
		(*float64)(nil), (*float64)(nil),
	}}
}

func explainRanker() *SlowQueries { return NewSlowQueries() }

// rankerWith retains endpoints directly, so the ranking can be driven without running
// SlowQueries' own samples.
func rankerWith(start, end []statementRow) *SlowQueries {
	sq := NewSlowQueries()

	if start != nil {
		sq.retainStart(start, false)
	}

	sq.retainEnd(end, false)

	return sq
}

func explainContext(index, total int) SampleContext {
	return SampleContext{
		At:             testWindowStart.Add(time.Duration(index) * time.Second),
		Index:          index,
		Total:          total,
		Database:       "orders_db",
		DBID:           "16401",
		HasGenericPlan: true,
		redact:         func(err error) string { return errorText(err, "") },
	}
}

// readableLog is a resolvable, readable stderr log holding the given content before the
// tail is opened.
func readableLog(t *testing.T, seed string) *fakeLogQuerier {
	t.Helper()

	dir := newLogDir(t)
	dir.writeCurrentLogfiles("stderr log/postgresql-2026-08-15_100224.log")
	dir.append(seed)

	return deniedQuerier(dir.settings())
}

// runExplainSamples drives both scheduled samples and returns the artifact's blocks.
func runExplainSamples(t *testing.T, e *Explain, q RowQuerier) []textBlock {
	t.Helper()

	var buf bytes.Buffer

	for index := 1; index <= 2; index++ {
		require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(index, 2)))
	}

	return parseTextArtifact(t, buf.String())
}

// summaryOf returns the last block, which is always the summary when one was written.
func summaryOf(t *testing.T, blocks []textBlock) textBlock {
	t.Helper()
	require.NotEmpty(t, blocks)

	last := blocks[len(blocks)-1]
	require.Equal(t, "true", last.fields["summary"], "the summary block closes the report")

	return last
}

func blockByQueryID(blocks []textBlock, queryid int64) (textBlock, bool) {
	want := strconv.FormatInt(queryid, 10)

	for _, block := range blocks {
		if block.fields["queryid"] == want {
			return block, true
		}
	}

	return textBlock{}, false
}

// --- the artifact and its wiring ---------------------------------------------

func TestExplainArtifact(t *testing.T) {
	artifact := NewExplain("", explainRanker()).Artifact()

	assert.Equal(t, "pg_explain", artifact.Name)
	assert.Equal(t, "pg_explain.txt", artifact.FileName)
	assert.Equal(t, StartEnd(), artifact.Schedule)

	assert.Equal(t, "database", artifact.Scope,
		"plans are obtainable only for the connected database, however cluster-wide the "+
			"ranking source is - the header must not claim coverage the file does not have")

	assert.Equal(t, formatText, artifact.Format,
		"plan bodies are multi-line and can legally contain a leading '#', so bytes= is the "+
			"only end marker a reader gets")
}

func TestExplainSampleBudgetIsModeDependent(t *testing.T) {
	assert.Equal(t, StatementTimeout, NewExplain("", explainRanker()).Artifact().SampleBudget,
		"a disabled feature must not buy every customer the enabled feature's worst case")

	for _, mode := range []string{ExplainModeLogged, ExplainModeAll} {
		assert.Equal(t, ExplainBudget+ExplainTimeout+StatementTimeout,
			NewExplain(mode, explainRanker()).Artifact().SampleBudget,
			"the aggregate, plus the candidate that can start just under it and still "+
				"run its full server-side timeout, plus the one pg_stat_activity read, "+
				"for mode %q", mode)
	}

	assert.Less(t, ExplainBudget, DefaultMaxExplains*ExplainTimeout,
		"were the aggregate equal to N x the per-candidate timeout, candidates_skipped_budget= "+
			"could never fire and would be decoration")
}

func TestNewExplainRefusesAWiringBug(t *testing.T) {
	assert.PanicsWithValue(t,
		"postgres: NewExplain requires the SlowQueries collector whose endpoints it ranks",
		func() { NewExplain(ExplainModeAll, nil) },
		"registration is one function, and a nil there is a bug every test catches")

	assert.PanicsWithValue(t,
		"postgres: NewExplain got an unvalidated explain mode: LOGGED",
		func() { NewExplain("LOGGED", explainRanker()) },
		"config lowercases and validates; treating an unknown mode as off would write "+
			"reason=explain_disabled about a run that asked for plans")

	assert.NotPanics(t, func() { NewExplain("", explainRanker()) },
		"the omitted key is the common case, not a bug")
}

// --- the no-plan outcomes ----------------------------------------------------

func TestExplainDisabledWritesOneReasonBlockAndReadsNothing(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, measuredPlan))

	blocks := runExplainSamples(t, NewExplain("", explainRanker()), q)

	require.Len(t, blocks, 1, "the opening sample writes nothing at all")
	assert.Equal(t, reasonExplainDisabled, blocks[0].fields["reason"])
	assert.Equal(t, "0", blocks[0].fields["bytes"])
	assert.Empty(t, blocks[0].body)

	assert.False(t, blocks[0].has("summary"),
		"there is no ranking to summarise, and a summary of zeroes would read as a "+
			"measurement of an idle database rather than a feature nobody turned on")

	assert.Empty(t, q.sql, "the disabled path is the default, so it must cost exactly one block")
	assert.Empty(t, q.submitted)
}

func TestExplainNoPlanOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    string
		querier func(t *testing.T) *fakeExplainConn
		assert  func(t *testing.T, block textBlock)
	}{
		{
			name: "log unresolved speaks the engine's own vocabulary",
			mode: ExplainModeLogged,
			querier: func(t *testing.T) *fakeExplainConn {
				q := newFakeExplainConn(deniedQuerier(logSettings{loggingCollector: "off", read: true}))
				q.activity = repeat(rowsResult(factsOnlyActivity()))

				return q
			},
			assert: func(t *testing.T, block textBlock) {
				assert.Equal(t, reasonCollectorOff, block.fields["log_reason"])
				assert.Equal(t, LogAccessNone, block.fields["log_access"])

				assert.False(t, block.has("plans_harvested"),
					"a zero beside a reason would let a receiver render an absence as a "+
						"measurement; the engine's own reason blocks omit matched= for this reason")

				assert.Equal(t, "true", block.fields["auto_explain_visible"],
					"the facts come through a LEFT JOIN, so they arrive even with no session to report")
			},
		},
		{
			name: "log readable and nothing matched is an observation, not a cause",
			mode: ExplainModeLogged,
			querier: func(t *testing.T) *fakeExplainConn {
				q := newFakeExplainConn(readableLog(t, measuredStatement+unrelatedTraffic))
				q.activity = repeat(rowsResult(factsOnlyActivity()))

				return q
			},
			assert: func(t *testing.T, block textBlock) {
				assert.Equal(t, "0", block.fields["plans_harvested"],
					"the file was read and held no plan - that is a measured zero")
				assert.Equal(t, LogAccessDirect, block.fields["log_access"])
				assert.False(t, block.has("log_reason"))
			},
		},
		{
			name: "a failed activity read drops every fact rather than defaulting one",
			mode: ExplainModeAll,
			querier: func(t *testing.T) *fakeExplainConn {
				q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
				q.activity = repeat(errResult(errors.New("ERROR: permission denied")))

				return q
			},
			assert: func(t *testing.T, block textBlock) {
				assert.False(t, block.has("auto_explain_visible"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := runExplainSamples(t, NewExplain(tc.mode, explainRanker()), tc.querier(t))

			require.Len(t, blocks, 1, "one summary block, and the opening sample writes nothing")

			block := blocks[0]
			assert.Equal(t, "true", block.fields["summary"])
			assert.Equal(t, reasonNoCandidates, block.fields["reason"])
			assert.Equal(t, "0", block.fields["bytes"])

			tc.assert(t, block)
		})
	}
}

func TestExplainOpensTheTailPastTheAgentsOwnFirstBurst(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, measuredPlan))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	e := NewExplain(ExplainModeLogged, explainRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))
	require.Empty(t, buf.String(), "arming writes nothing")

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	assert.Equal(t, "0", summaryOf(t, parseTextArtifact(t, buf.String())).fields["plans_harvested"],
		"a plan logged before the tail was opened belongs to a window that did not happen")
}

func TestExplainOnAOneTickWindowStillReports(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, measuredPlan))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	e := NewExplain(ExplainModeLogged, explainRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 1)))

	summary := summaryOf(t, parseTextArtifact(t, buf.String()))

	assert.Equal(t, "0", summary.fields["plans_harvested"],
		"opened at EOF and read in the same breath, so the window covers no interval at all")
	assert.Equal(t, LogAccessDirect, summary.fields["log_access"])
}

// --- the ranking -------------------------------------------------------------

func TestExplainRanksByTheDeltaNotTheTotal(t *testing.T) {
	require.Greater(t, ordersInventoryEnd.execTime, ordersItemsEnd.execTime)
	require.Greater(t,
		ordersItemsEnd.execTime-ordersItemsStart.execTime,
		ordersInventoryEnd.execTime-ordersInventoryStart.execTime)

	sq := rankerWith(
		[]statementRow{pg18Statement(ordersItemsStart), pg18Statement(ordersInventoryStart)},
		[]statementRow{pg18Statement(ordersItemsEnd), pg18Statement(ordersInventoryEnd)},
	)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	require.Len(t, blocks, 3, "two candidates and the summary")

	assert.Equal(t, strconv.FormatInt(ordersItemsEnd.queryid, 10), blocks[0].fields["queryid"])
	assert.Equal(t, "1", blocks[0].fields["rank"])
	assert.Equal(t, "2", blocks[1].fields["rank"])

	assert.Equal(t, rankingStatementsDelta, summaryOf(t, blocks).fields["ranking"])

	for _, block := range blocks {
		assert.NotContains(t, block.header, "delta=",
			"the delta is an in-memory sort key, never a written measurement")
	}
}

func TestExplainValidityRules(t *testing.T) {
	restarted := pg18Statement(ordersItemsEnd)
	restarted.totalExecTime = ptr(12.5)
	movedSince := testStatsSince.Add(time.Hour)
	restarted.statsSince = &movedSince

	negative := pre17Statement(ordersItemsEnd)
	negative.totalExecTime = ptr(1.0)

	for _, tc := range []struct {
		name     string
		endpoint func() *SlowQueries
		counter  string
		written  string
	}{
		{
			name: "a moved stats_since ranks by the end value and is counted",
			endpoint: func() *SlowQueries {
				return rankerWith([]statementRow{pg18Statement(ordersItemsStart)},
					[]statementRow{restarted})
			},
			counter: "candidates_restarted",
			written: "1",
		},
		{
			name: "a negative delta the extension cannot explain is excluded",
			endpoint: func() *SlowQueries {
				return rankerWith([]statementRow{pre17Statement(ordersItemsStart)},
					[]statementRow{negative})
			},
			counter: "candidates_invalid",
		},
		{
			name: "a row only in the end read is zero-baselined when neither endpoint truncated",
			endpoint: func() *SlowQueries {
				return rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})
			},
			written: "1",
		},
		{
			name: "and is unrankable when an endpoint was truncated",
			endpoint: func() *SlowQueries {
				sq := NewSlowQueries()
				sq.retainStart(nil, true)
				sq.retainEnd([]statementRow{pg18Statement(ordersItemsEnd)}, false)

				return sq
			},
			counter: "candidates_unrankable",
		},
		{
			name: "and is unrankable when the opening endpoint was never read",
			endpoint: func() *SlowQueries {
				sq := NewSlowQueries()
				sq.retainEnd([]statementRow{pg18Statement(ordersItemsEnd)}, false)

				return sq
			},
			counter: "candidates_unrankable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
			q.activity = repeat(rowsResult(factsOnlyActivity()))

			blocks := runExplainSamples(t, NewExplain(ExplainModeAll, tc.endpoint()), q)
			summary := summaryOf(t, blocks)

			if tc.counter != "" {
				assert.Equal(t, "1", summary.fields[tc.counter])
			}

			if tc.written == "" {
				assert.False(t, summary.has("candidates_written"))
				assert.Equal(t, rankingActivityFallback, summary.fields["ranking"],
					"nothing rankable survived, which is one of the fallback's three triggers")

				return
			}

			assert.Equal(t, tc.written, summary.fields["candidates_written"])
			assert.Equal(t, rankingStatementsDelta, summary.fields["ranking"])
		})
	}
}

func TestExplainDiscardsEveryDeltaWhenStatsWereReset(t *testing.T) {
	sq := rankerWith(
		[]statementRow{pg18Statement(ordersItemsStart)},
		[]statementRow{pg18Statement(ordersItemsEnd)},
	)

	moved := testInfoStatsReset.Add(time.Minute)
	sq.retained.startInfo = &infoRow{statsReset: &testInfoStatsReset}
	sq.retained.endInfo = &infoRow{statsReset: &moved}

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	assert.Equal(t, rankingActivityFallback, summaryOf(t, blocks).fields["ranking"],
		"the two info reads bracket the window; a reset between them spans every delta in the file")
}

// --- the prefilter and the allowlist -----------------------------------------

func TestExplainPrefilterCountsEachClass(t *testing.T) {
	otherDatabase := pg18Statement(ordersInventoryEnd)
	otherDatabase.dbid = ptr("16999")

	self := pg18Statement(agentRead)
	self.userid = ptr(explainSelfOID)

	nested := pg18Statement(ordersUpdateEnd)
	nested.toplevel = ptr(false)

	utility := pg18Statement(ordersItemsEnd)
	utility.query = ptr("VACUUM ANALYZE orders")

	sq := rankerWith([]statementRow{}, []statementRow{
		maskedStatement(ordersItemsEnd), otherDatabase, self, nested, utility,
	})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	summary := summaryOf(t, runExplainSamples(t, NewExplain(ExplainModeAll, sq), q))

	assert.Equal(t, "5", summary.fields["candidates_considered"])
	assert.Equal(t, "1", summary.fields["excluded_masked"])
	assert.Equal(t, "1", summary.fields["excluded_other_database"])
	assert.Equal(t, "1", summary.fields["excluded_self"])
	assert.Equal(t, "1", summary.fields["excluded_not_toplevel"])
	assert.Equal(t, "1", summary.fields["excluded_utility"])

	assert.Empty(t, q.submitted, "an excluded candidate is never submitted")
}

func TestExplainAllowlist(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{
		{query: "SELECT 1", want: true},
		{query: "  select 1", want: true},
		{query: "(SELECT 1) UNION (SELECT 2)", want: true},
		{query: "WITH t AS (SELECT 1) SELECT * FROM t", want: true},
		{query: "INSERT INTO orders VALUES (1)", want: true},
		{query: "MERGE INTO orders USING src ON true", want: true},
		{query: "TABLE orders", want: true},
		{query: "VALUES (1)", want: true},

		{query: "-- a leading comment\nSELECT 1", want: true},
		{query: "/* block */ SELECT 1", want: true},
		{query: "/* outer /* nested */ still outer */ SELECT 1", want: true},

		{query: "EXECUTE plan_a", want: false},
		{query: "EXPLAIN (VERBOSE) SELECT 1", want: false},
		{query: "CALL do_work()", want: false},
		{query: "COPY orders TO stdout", want: false},
		{query: "DO $$ BEGIN END $$", want: false},
		{query: "SET work_mem = '64MB'", want: false},
		{query: "PREPARE p AS SELECT 1", want: false},
		{query: "FETCH ALL FROM c", want: false},
		{query: "VACUUM ANALYZE orders", want: false},
		{query: "CREATE TABLE t AS SELECT 1", want: false},
		{query: "/* unterminated SELECT 1", want: false},
		{query: "-- only a comment", want: false},
		{query: "", want: false},
	} {
		t.Run(fmt.Sprintf("%.40q", tc.query), func(t *testing.T) {
			assert.Equal(t, tc.want, explainable(tc.query))
		})
	}
}

// --- the two estimated modes -------------------------------------------------

func TestExplainLiteralTierUsesTheExtendedProtocol(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	require.Len(t, blocks, 2)
	block := blocks[0]

	assert.Equal(t, planModeEstimatedLiteral, block.fields["mode"])
	assert.Equal(t, candidateSourceStatements, block.fields["candidate_source"])
	assert.Equal(t, explainSearchPath, block.fields["search_path"])
	assert.NotEmpty(t, block.body)

	require.Len(t, q.submitted, 1)
	assert.Contains(t, q.submitted[0], "SELECT * FROM order_items WHERE order_id = 4021",
		"the activity text, which is the only place literal values exist")

	require.Len(t, q.protocols, 1)
	assert.Equal(t, protocolExtended, q.protocols[0],
		"one-shot, so customer SQL never enters pgx's statement cache, and it refuses "+
			"the multi-statement batch a simple-protocol client's activity text can be")

	assert.Equal(t, []string{setExplainTimeoutSQL, resetExplainTimeoutSQL}, q.utility,
		"the bound is server-side: a client-context expiry would close the shared connection")
}

func TestExplainGenericTierUsesTheSimpleProtocol(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersUpdateEnd)})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	require.Len(t, blocks, 2)
	assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"])

	require.Len(t, q.submitted, 1)
	assert.Contains(t, q.submitted[0], "GENERIC_PLAN")
	assert.Contains(t, q.submitted[0], ordersUpdateEnd.query, "the normalized text, $n and all")

	require.Len(t, q.protocols, 1)
	assert.Equal(t, protocolSimple, q.protocols[0],
		"extended protocol fails an unbound $1 at Bind, before the server sees the statement")
}

func TestExplainParameterizedActivityTextReroutesToGeneric(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
		pid:     4021,
		queryID: ptr(ordersItemsEnd.queryid),
		state:   "active",

		query:      "SELECT * FROM order_items WHERE order_id = $1",
		runningFor: 9.5,
	})}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"])
	assert.Equal(t, protocolSimple, q.protocols[0])
}

func TestExplainTruncatedActivityTextIsNotSubmitted(t *testing.T) {
	end := pg18Statement(ordersItemsEnd)
	end.query = ptr("SELECT * FROM order_items WHERE order_id = $1")

	sq := rankerWith([]statementRow{}, []statementRow{end})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
		pid:     4021,
		queryID: ptr(ordersItemsEnd.queryid),
		state:   "active",
		query:   strings.Repeat("x", 1023),

		queryBytes: 1023,
		runningFor: 9.5,
	})}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"],
		"truncated activity text falls through to the normalized form, which is intact")
	assert.NotContains(t, q.submitted[0], "xxxx",
		"the bytes the server cut mid-token are never submitted")
}

func TestExplainTruncatedWithNoOtherTextIsExcluded(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
		pid: 4021, state: "active", runningFor: 9.5,

		query:      "SELECT * FROM order_items WHERE note = " + strings.Repeat("x", 980),
		queryBytes: 1023,
	})}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, explainRanker()), q)

	assert.Equal(t, planModeNone, blocks[0].fields["mode"])
	assert.Equal(t, reasonQueryTruncated, blocks[0].fields["reason"])
	assert.Empty(t, q.submitted, "a statement cut mid-token is a guaranteed syntax error")
}

func TestExplainGenericTierIsAbsentBelowPostgres16(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersUpdateEnd)})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	e := NewExplain(ExplainModeAll, sq)

	s := explainContext(2, 2)
	s.HasGenericPlan = false

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))
	require.NoError(t, e.Sample(context.Background(), q, &buf, s))

	blocks := parseTextArtifact(t, buf.String())

	assert.Equal(t, reasonGenericPlanUnsupported, blocks[0].fields["reason"])
	assert.Empty(t, q.submitted,
		"the gate is a capability decided before the statement is built: on 14/15 the "+
			"error would be the same one an unbound parameter produces")
}

func TestExplainRecordsThePlanIdentifierPerTier(t *testing.T) {
	t.Run("literal asserts equality", func(t *testing.T) {
		sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.defaultPlan = rowsResult(planRows(
			"Seq Scan on public.order_items  (cost=0.00..8420.00 rows=3 width=64)",
			"Query Identifier: "+strconv.FormatInt(ordersItemsEnd.queryid, 10),
		))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

		assert.Equal(t, strconv.FormatInt(ordersItemsEnd.queryid, 10), blocks[0].fields["plan_queryid"])
		assert.Equal(t, "true", blocks[0].fields["queryid_match"])
	})

	t.Run("a mismatch is flagged, not hidden", func(t *testing.T) {
		sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.defaultPlan = rowsResult(planRows("Query Identifier: 42"))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

		assert.Equal(t, "42", blocks[0].fields["plan_queryid"])
		assert.Equal(t, "false", blocks[0].fields["queryid_match"],
			"the one machine-checkable symptom of the agent's session resolving a different object")
	})

	t.Run("generic asserts nothing", func(t *testing.T) {
		sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersUpdateEnd)})

		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.activity = repeat(rowsResult(factsOnlyActivity()))
		q.defaultPlan = rowsResult(planRows("Query Identifier: 3310092847719224551"))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

		assert.Equal(t, "3310092847719224551", blocks[0].fields["plan_queryid"])
		assert.False(t, blocks[0].has("queryid_match"),
			"the $n text jumbles a Param where the original jumbled a Const, so the "+
				"identifier differs by construction and a reader must not read that as corruption")
	})
}

func TestExplainPerCandidateErrorLeavesTheRestIntact(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{
		pg18Statement(ordersItemsEnd), pg18Statement(ordersInventoryEnd),
	})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))
	q.plans = map[string]fakeResult{
		ordersItemsEnd.query: errResult(errors.New(
			"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")),
	}

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, sq), q)

	require.Len(t, blocks, 3)

	failed, ok := blockByQueryID(blocks, ordersItemsEnd.queryid)
	require.True(t, ok)
	assert.Contains(t, failed.fields["error"], "57014")
	assert.Equal(t, planModeNone, failed.fields["mode"])
	assert.Equal(t, "0", failed.fields["bytes"])

	survived, ok := blockByQueryID(blocks, ordersInventoryEnd.queryid)
	require.True(t, ok)
	assert.Equal(t, planModeEstimatedGeneric, survived.fields["mode"])
	assert.NotEmpty(t, survived.body,
		"a server-side timeout is a per-candidate error; the connection is still usable")
}

func TestExplainCutsAtTheCandidateCap(t *testing.T) {
	var end []statementRow

	for i := range DefaultMaxExplains + 5 {
		row := pg18Statement(ordersItemsEnd)
		row.queryid = ptr(int64(1000 + i))
		row.totalExecTime = ptr(float64(1000 - i))
		end = append(end, row)
	}

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, rankerWith([]statementRow{}, end)), q)

	require.Len(t, blocks, DefaultMaxExplains+1)

	summary := summaryOf(t, blocks)
	assert.Equal(t, strconv.Itoa(DefaultMaxExplains+5), summary.fields["candidates_considered"])
	assert.Equal(t, strconv.Itoa(DefaultMaxExplains), summary.fields["candidates_written"],
		"fewer plans than candidates is a bounded selection, and the counters say where it fell")
}

func TestExplainStopsWhenTheAggregateBudgetIsSpent(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{
		pg18Statement(ordersItemsEnd), pg18Statement(ordersInventoryEnd),
	})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	e := NewExplain(ExplainModeAll, sq)

	ticks := []time.Time{
		testWindowStart,
		testWindowStart,
		testWindowStart.Add(ExplainBudget + time.Second),
	}
	e.now = func() time.Time {
		next := ticks[0]
		if len(ticks) > 1 {
			ticks = ticks[1:]
		}

		return next
	}

	blocks := runExplainSamples(t, e, q)

	assert.Equal(t, "1", summaryOf(t, blocks).fields["candidates_skipped_budget"])
	assert.Equal(t, reasonBudgetSpent, blocks[1].fields["reason"])
	assert.Len(t, q.submitted, 1)
}

// --- the activity fallback ---------------------------------------------------

func TestExplainActivityFallbackRanksStateAware(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{
		activityValue(activityFixture{
			pid: 11, state: "idle", query: "SELECT 1 FROM idle_but_ancient",

			runningFor: 10800, ranFor: 0.002,
		}),
		activityValue(activityFixture{
			pid: 12, state: "active", query: "SELECT 2 FROM still_running",
			runningFor: 9.5,
		}),
		activityValue(activityFixture{
			pid: 13, state: "idle", query: "SELECT 3 FROM finished_slowly",
			runningFor: 4000, ranFor: 41.5,
		}),
	}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, explainRanker()), q)

	require.Len(t, blocks, 4)

	assert.Equal(t, rankingActivityFallback, summaryOf(t, blocks).fields["ranking"])

	assert.Equal(t, "12", blocks[0].fields["pid"], "a running statement's clock has not stopped")
	assert.Equal(t, "13", blocks[1].fields["pid"], "then the slowest completed statement")
	assert.Equal(t, "11", blocks[2].fields["pid"])

	for _, block := range blocks[:3] {
		assert.Equal(t, candidateSourceActivity, block.fields["candidate_source"])
		assert.NotEmpty(t, block.fields["query_start"])
	}
}

func TestExplainFallbackCarriesTheQueryIDWhenTheServerHasOne(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{
		activityValue(activityFixture{
			pid: 11, queryID: ptr(int64(884210)), state: "active",
			query: "SELECT 1 FROM t", runningFor: 3,
		}),
		activityValue(activityFixture{
			pid: 12, state: "active", query: "SELECT 2 FROM t", runningFor: 2,
		}),
	}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, explainRanker()), q)

	assert.Equal(t, "884210", blocks[0].fields["query_id"],
		"a database with no extension view still computes ids when the library is preloaded")
	assert.False(t, blocks[1].has("query_id"))
	assert.False(t, blocks[0].has("queryid"),
		"queryid= names a pg_stat_statements row; this candidate is a session")
}

// --- the invariant -----------------------------------------------------------

func TestExplainOptionListNeverContainsAnalyze(t *testing.T) {
	for _, generic := range []bool{false, true} {
		options := explainOptions(generic)

		assert.NotContains(t, options, "ANALYZE",
			"EXPLAIN ANALYZE executes the statement, and the top-ranked candidate is by "+
				"construction the most expensive query on the server")
		assert.Contains(t, options, "VERBOSE")
		assert.Contains(t, options, "SETTINGS")
		assert.Equal(t, generic, slices.Contains(options, "GENERIC_PLAN"))
	}

	assert.Equal(t, "EXPLAIN (VERBOSE, SETTINGS) SELECT analyze_runs FROM audit",
		explainStatement(explainOptions(false), "SELECT analyze_runs FROM audit"))
}

func TestExplainSubmitsNoStatementUnderModeLogged(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	blocks := runExplainSamples(t, NewExplain(ExplainModeLogged, sq), q)

	require.Len(t, blocks, 2)
	assert.Equal(t, planModeNone, blocks[0].fields["mode"])
	assert.Equal(t, reasonNoLoggedPlan, blocks[0].fields["reason"])

	assert.Empty(t, q.submitted, "mode logged reads the server's own log and writes nothing back")
	assert.Empty(t, q.utility, "and never touches statement_timeout")
}

// --- framing -----------------------------------------------------------------

func TestExplainEveryBlockCarriesBytes(t *testing.T) {
	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

	for _, mode := range []string{"", ExplainModeLogged, ExplainModeAll} {
		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))

		for _, block := range runExplainSamples(t, NewExplain(mode, sq), q) {
			assert.True(t, block.has("bytes"), "mode %q wrote a block with no bytes=", mode)
			assert.Equal(t, strconv.Itoa(len(block.body)), block.fields["bytes"])
		}
	}
}

func TestExplainWriteFailureIsAnErrorNotAPartialBlock(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	sq := rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})

	for _, mode := range []string{"", ExplainModeLogged, ExplainModeAll} {
		e := NewExplain(mode, sq)
		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))

		require.NoError(t, e.Sample(context.Background(), q, &bytes.Buffer{}, explainContext(1, 2)))

		err := e.Sample(context.Background(), q, failingWriter{err: sinkErr}, explainContext(2, 2))

		require.ErrorIs(t, err, sinkErr, "mode %q swallowed a failed write", mode)
	}
}

// explainGoldenClock is goldenClock plus one read: Explain implements Closing, and the
// window reads the clock for the closing pass whether or not that pass writes anything.
func explainGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 112),
		at(32, 5, 112),
		at(34, 5, 140),
		at(34, 5, 180),
		at(34, 5, 201),
	)
}

func runExplainWindow(t *testing.T, e *Explain, clock *scriptedClock, conn windowConn) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{e},
		now:        clock.now,
		after:      clock.after,
		connect:    connectTo(conn),
	}

	return window.Run(context.Background())
}

func TestExplainGoldenDisabled(t *testing.T) {
	results := runExplainWindow(t, NewExplain("", explainRanker()), explainGoldenClock(t), newFakeWindowConn())

	require.Equal(t, StatusComplete, results[0].Status)
	require.Equal(t, 2, results[0].SamplesWritten,
		"both scheduled samples ran; only one of them had anything to say")

	assert.Equal(t, bloatGolden(t, "pg_explain_disabled.txt"), artifactText(t, results[0]))
}

// --- goldens -----------------------------------------------------------------

// explainWindowConn is a window connection whose collector traffic goes to the explain
// fake and whose identify goes to the window fake.
type explainWindowConn struct {
	*fakeWindowConn

	q *fakeExplainConn
}

func (c *explainWindowConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == currentDatabaseSQL {
		return c.fakeWindowConn.QueryRow(ctx, sql, args...)
	}

	return c.q.QueryRow(ctx, sql, args...)
}

func (c *explainWindowConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.q.Query(ctx, sql, args...)
}

// Forwarded explicitly: the generic mode reaches for this through an optional interface.
func (c *explainWindowConn) ExecSimple(ctx context.Context, sql string, maxBytes int) (
	[]string, bool, error,
) {
	return c.q.ExecSimple(ctx, sql, maxBytes)
}

func explainConnFor(t *testing.T, q *fakeExplainConn, hasGenericPlan bool) *explainWindowConn {
	t.Helper()

	window := newFakeWindowConn()
	window.hasGenericPlan = hasGenericPlan

	return &explainWindowConn{fakeWindowConn: window, q: q}
}

func TestExplainGoldenFull(t *testing.T) {
	otherDatabase := pg18Statement(agentRead)
	otherDatabase.dbid = ptr("16999")

	sq := rankerWith(
		[]statementRow{pg18Statement(ordersItemsStart), pg18Statement(ordersInventoryStart),
			pg18Statement(ordersUpdateStart)},
		[]statementRow{pg18Statement(ordersItemsEnd), pg18Statement(ordersInventoryEnd),
			pg18Statement(ordersUpdateEnd), otherDatabase},
	)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.defaultPlan = rowsResult(planRows(
		" Index Scan using inventory_sku_idx on public.inventory  (cost=0.42..8.44 rows=1 width=48)",
		"   Index Cond: (inventory.sku = 'SKU-88291'::text)",
	))
	q.plans = map[string]fakeResult{
		"order_items": rowsResult(planRows(
			" Seq Scan on public.order_items  (cost=0.00..8420.00 rows=3 width=64)",
			"   Filter: (order_id = 4021)",
			" Query Identifier: "+strconv.FormatInt(ordersItemsEnd.queryid, 10),
		)),
		ordersUpdateEnd.query: errResult(errors.New("ERROR: permission denied for table orders")),
	}

	results := runExplainWindow(t, NewExplain(ExplainModeAll, sq), explainGoldenClock(t),
		explainConnFor(t, q, true))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_full.txt"), artifactText(t, results[0]))
}

func TestExplainGoldenLeastPrivilege(t *testing.T) {
	sq := rankerWith(
		[]statementRow{pg18Statement(ordersItemsStart), pg18Statement(ordersInventoryStart)},
		[]statementRow{pg18Statement(ordersItemsEnd), pg18Statement(ordersInventoryEnd)},
	)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.defaultPlan = errResult(errors.New("ERROR: permission denied for table order_items"))

	results := runExplainWindow(t, NewExplain(ExplainModeAll, sq), explainGoldenClock(t),
		explainConnFor(t, q, true))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_least_privilege.txt"), artifactText(t, results[0]))
}

func TestExplainGoldenActivityFallback(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{
		activityValue(activityFixture{
			pid: 4021, queryID: ptr(int64(884210)), state: "active",
			query: "SELECT * FROM inventory WHERE sku = 'SKU-88291'", runningFor: 41.5,
		}),
		activityValue(activityFixture{
			pid: 4102, state: "idle",
			query: "SELECT count(*) FROM order_items", runningFor: 900, ranFor: 12.25,
		}),
	}))

	q.plans = map[string]fakeResult{
		"inventory": rowsResult(planRows(
			" Index Scan using inventory_sku_idx on public.inventory  (cost=0.42..8.44 rows=1 width=48)",
			"   Index Cond: (inventory.sku = 'SKU-88291'::text)",
		)),
		"count(*)": rowsResult(planRows(
			" Aggregate  (cost=25.00..25.01 rows=1 width=8)",
			"   ->  Seq Scan on public.order_items  (cost=0.00..22.00 rows=1200 width=0)",
		)),
	}

	results := runExplainWindow(t, NewExplain(ExplainModeAll, explainRanker()), explainGoldenClock(t),
		explainConnFor(t, q, true))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_activity_fallback.txt"), artifactText(t, results[0]))
}

func TestExplainGoldenPre16(t *testing.T) {
	sq := rankerWith(
		[]statementRow{pre17Statement(ordersUpdateStart)},
		[]statementRow{pre17Statement(ordersUpdateEnd)},
	)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	results := runExplainWindow(t, NewExplain(ExplainModeAll, sq), explainGoldenClock(t),
		explainConnFor(t, q, false))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_pre16.txt"), artifactText(t, results[0]))
}

// --- the LOGGED mode ---------------------------------------------------------

// planEntry is auto_explain's measured stderr shape; an empty queryID is what
// log_verbose=off produces, and what PostgreSQL 14/15 produce even with it on.
func planEntry(queryID, duration, query string) string {
	entry := "2026-08-17 02:01:31.226 UTC [13031] LOG:  duration: " + duration + " ms  plan:\n" +
		"\tQuery Text: " + query + "\n" +
		"\tSeq Scan on public.order_items  (cost=0.00..8420.00 rows=3 width=64)\n"

	if queryID != "" {
		entry += "\tQuery Identifier: " + queryID + "\n"
	}

	return entry
}

func itemsPlanEntry(duration string) string {
	return planEntry(strconv.FormatInt(ordersItemsEnd.queryid, 10), duration,
		"SELECT * FROM order_items WHERE order_id = $1")
}

func readableLogAs(t *testing.T, format logFormat, seed string) *fakeLogQuerier {
	t.Helper()

	dir := newLogDir(t)

	// newLogDir always makes the stderr file; only one generation may exist, or
	// currentLogPath cannot tell which one the tail is reading.
	require.NoError(t, os.Remove(dir.path))

	dir.format = format
	dir.path = filepath.Join(dir.logDirectory, "postgresql-2026-08-15_100224"+formatExtension(format))

	require.NoError(t, os.WriteFile(dir.path, nil, 0o644))
	dir.writeCurrentLogfiles(string(format) + " log/" + filepath.Base(dir.path))
	dir.append(seed)

	return deniedQuerier(dir.settings())
}

func itemsRanker() *SlowQueries {
	return rankerWith([]statementRow{}, []statementRow{pg18Statement(ordersItemsEnd)})
}

func TestExplainLoggedTierAttachesByIdentifier(t *testing.T) {
	entry := itemsPlanEntry("0.023")

	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeAll, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q), entry+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 2)

	block := blocks[0]
	assert.Equal(t, planModeLogged, block.fields["mode"])
	assert.Equal(t, matchedByMessage, block.fields["matched_by"],
		"written by this collector on all three formats; the engine would say sqlstate "+
			"on csvlog and jsonlog, where 00000 is every LOG line in the file")
	assert.Equal(t, strconv.FormatInt(ordersItemsEnd.queryid, 10), block.fields["plan_queryid"])
	assert.Equal(t, "true", block.fields["queryid_match"])
	assert.False(t, block.has("plans_seen"), "one execution is the uninteresting case")
	assert.Equal(t, entry, block.body, "the server's bytes, verbatim")

	assert.Empty(t, q.submitted,
		"the server's own plan for the execution that happened beats one the agent's "+
			"session would produce, so this candidate is never submitted")

	summary := summaryOf(t, blocks)
	assert.Equal(t, "1", summary.fields["plans_harvested"])
	assert.Equal(t, "1", summary.fields["plans_written"])
}

// currentLogPath is where readableLog's tail is reading from.
func currentLogPath(t *testing.T, q *fakeExplainConn) string {
	t.Helper()

	settings := q.settings
	entries, err := filepath.Glob(filepath.Join(settings.dataDirectory, "log", "postgresql-*"))
	require.NoError(t, err)
	require.Len(t, entries, 1)

	return entries[0]
}

func TestExplainLoggedKeepsTheSlowestPerIdentifier(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeAll, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q),
		itemsPlanEntry("0.9")+itemsPlanEntry("412.5")+itemsPlanEntry("7.25")+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())

	assert.Contains(t, blocks[0].body, "duration: 412.5 ms",
		"the pathological execution's plan is the evidence")
	assert.Equal(t, "3", blocks[0].fields["plans_seen"])

	summary := summaryOf(t, blocks)
	assert.Equal(t, "3", summary.fields["plans_harvested"])
	assert.Equal(t, "1", summary.fields["plans_written"])
	assert.Equal(t, "2", summary.fields["plans_dropped"])
}

func TestExplainLoggedWithNoIdentifierIsWrittenUnattached(t *testing.T) {
	entry := planEntry("", "12.5", "SELECT * FROM order_items WHERE order_id = $1")

	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q), entry+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 3, "the candidate, the unattached plan, the summary")

	assert.Equal(t, reasonNoLoggedPlan, blocks[0].fields["reason"],
		"nothing could be attached to it, and matching on query text is the "+
			"normalization the plan refused")

	unattached := blocks[1]
	assert.Equal(t, planModeLogged, unattached.fields["mode"])
	assert.Empty(t, unattached.fields["queryid"])
	assert.True(t, unattached.has("queryid"), "present and empty: no row, rather than nobody looked")
	assert.Equal(t, reasonNoQueryIdentifier, unattached.fields["reason"])
	assert.Equal(t, entry, unattached.body)
}

func TestExplainLoggedIdentifierMatchingNoCandidateIsNotWritten(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q),
		planEntry("99887766", "31.2", "SELECT 1 FROM somewhere_else")+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 2, "the candidate and the summary; the plan is outside a ranked report")

	summary := summaryOf(t, blocks)
	assert.Equal(t, "1", summary.fields["plans_harvested"])
	assert.False(t, summary.has("plans_written"),
		"plans_harvested minus plans_written is where a plan nobody claimed shows")
}

func TestExplainLoggedNonTextFormatIsStoredButNeverAttached(t *testing.T) {
	entry := "2026-08-17 02:01:31.226 UTC [13031] LOG:  duration: 8.100 ms  plan:\n" +
		"\t{\n" +
		"\t  \"Query Text\": \"SELECT * FROM order_items WHERE order_id = $1\",\n" +
		"\t  \"Plan\": {\"Node Type\": \"Seq Scan\", \"Relation Name\": \"order_items\"}\n" +
		"\t}\n"

	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q), entry+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 3)

	assert.Equal(t, reasonNoQueryIdentifier, blocks[1].fields["reason"])
	assert.Equal(t, entry, blocks[1].body, "stored verbatim - the promise is the server's bytes")
}

func TestExplainLoggedCaps(t *testing.T) {
	t.Run("identifier-less entries keep the newest DefaultMaxUnattachedPlans", func(t *testing.T) {
		var log strings.Builder
		for i := range DefaultMaxUnattachedPlans + 5 {
			log.WriteString(planEntry("", "1.0", fmt.Sprintf("SELECT %d", i)))
		}

		log.WriteString(unrelatedTraffic)

		store := planStoreOf(t, log.String())

		assert.Equal(t, DefaultMaxUnattachedPlans+5, store.total)
		assert.Len(t, store.unattached(), DefaultMaxUnattachedPlans)
		assert.Equal(t, 5, store.dropped)

		assert.Contains(t, string(store.unattached()[0].body), "SELECT 5",
			"oldest-first eviction, so the newest survive")
	})

	t.Run("the byte budget evicts oldest-first", func(t *testing.T) {
		store := &planStore{
			byID: map[string]*loggedPlan{}, seen: map[string]int{}, claimed: map[string]bool{},
		}

		big := make([]byte, MaxRetainedPlanBytes/2+1)
		for i := range big {
			big[i] = 'x'
		}

		for i := range 3 {
			store.add([]byte("id" + strconv.Itoa(i) + string(big)))
		}

		assert.Equal(t, 3, store.total)
		assert.Len(t, store.retained, 1)
		assert.Equal(t, 2, store.dropped)
		assert.LessOrEqual(t, store.bytes, MaxRetainedPlanBytes)
	})
}

func planStoreOf(t *testing.T, log string) *planStore {
	t.Helper()

	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, explainRanker())
	require.True(t, e.tail.openAtEnd(context.Background(), q, explainContext(1, 2)))

	defer e.tail.closeFile()

	appendFile(t, currentLogPath(t, q), log)

	events, _ := e.tail.readEvents(context.Background(), q, time.Time{})

	return newPlanStore(events)
}

func TestExplainLoggedOnEveryLogFormat(t *testing.T) {
	entry := itemsPlanEntry("0.023")
	message := stderrMessage(entry)

	for _, tc := range []struct {
		format logFormat
		body   func(t *testing.T) string
	}{
		{format: logFormatStderr, body: func(*testing.T) string { return entry }},
		{format: logFormatCSV, body: func(*testing.T) string { return csvEntry(message) }},
		{format: logFormatJSON, body: func(t *testing.T) string { return jsonEntryLine(t, message) }},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			q := newFakeExplainConn(readableLogAs(t, tc.format, ""))

			e := NewExplain(ExplainModeLogged, itemsRanker())

			var buf bytes.Buffer
			require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

			appendFile(t, currentLogPath(t, q), tc.body(t))

			require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

			blocks := parseTextArtifact(t, buf.String())

			assert.Equal(t, planModeLogged, blocks[0].fields["mode"], "the %s path", tc.format)
			assert.Equal(t, matchedByMessage, blocks[0].fields["matched_by"],
				"one token on every format: the message predicate is what selected this")
		})
	}
}

// --- the closing ------------------------------------------------------------

func TestExplainClosingShipsEvidenceFromACancelledWindow(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q),
		itemsPlanEntry("55.5")+planEntry("", "3.0", "SELECT 1")+unrelatedTraffic)

	require.NoError(t, e.WriteClosing(&buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 3, "both plans, plus the summary")

	assert.Equal(t, strconv.FormatInt(ordersItemsEnd.queryid, 10), blocks[0].fields["plan_queryid"])
	assert.Equal(t, reasonNoRankedReport, blocks[0].fields["reason"],
		"it has an identifier; what it has no candidate to attach to is a report that never ran")

	assert.Equal(t, reasonNoQueryIdentifier, blocks[1].fields["reason"])

	summary := summaryOf(t, blocks)
	assert.Equal(t, "2", summary.fields["plans_harvested"])
	assert.Equal(t, "2", summary.fields["plans_written"])
}

func TestExplainClosingIsSilentAfterAClosingSample(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeLogged, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q), itemsPlanEntry("55.5")+unrelatedTraffic)

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	written := buf.Len()
	require.NoError(t, e.WriteClosing(&buf, explainContext(2, 2)))

	assert.Equal(t, written, buf.Len(),
		"everything the closing pass would emit is already in the file")
}

func TestExplainClosingIsSilentWhenDisabled(t *testing.T) {
	e := NewExplain("", explainRanker())

	var buf bytes.Buffer
	require.NoError(t, e.WriteClosing(&buf, explainContext(2, 2)))

	assert.Empty(t, buf.String(), "no tail was ever opened, so there is nothing to drain")
}

// --- the logged goldens ------------------------------------------------------

// explainLoggedWindow runs a full window whose log grows between the two samples.
func explainLoggedWindow(t *testing.T, e *Explain, q *fakeExplainConn, entries string) []ArtifactResult {
	t.Helper()

	path := currentLogPath(t, q)

	writer := newFakeCollector("pg_log_writer")
	writer.artifact.Schedule = Every(DefaultLogTailInterval)

	tick := 0
	writer.sample = func(_ context.Context, s SampleContext, w io.Writer) error {
		if tick == 1 {
			appendFile(t, path, entries)
		}

		tick++

		return writeBlockHeader(w, "pg_log_writer", "cluster",
			[]headerField{{"sample", strconv.Itoa(s.Index)}}, s.At)
	}

	clock := logGoldenClock(t, 12)
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{e, writer},
		now:        clock.now,
		after:      clock.after,
		connect:    connectTo(explainConnFor(t, q, true)),
	}

	return window.Run(context.Background())
}

func TestExplainGoldenLogged(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	results := explainLoggedWindow(t, NewExplain(ExplainModeLogged, itemsRanker()), q,
		itemsPlanEntry("0.9")+itemsPlanEntry("412.5")+unrelatedTraffic)

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_logged.txt"), artifactText(t, results[0]))
}

func TestExplainGoldenLoggedNoIdentifier(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	results := explainLoggedWindow(t, NewExplain(ExplainModeLogged, itemsRanker()), q,
		planEntry("", "412.5", "SELECT * FROM order_items WHERE order_id = $1")+unrelatedTraffic)

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_explain_logged_noid.txt"), artifactText(t, results[0]))
}

// --- review fixes ------------------------------------------------------------
//
// Each test below pins one defect found in the implementation review of 2026-08-23.

func TestExplainLiteralTierJoinsOnTheFullIdentity(t *testing.T) {
	other := activityValue(activityFixture{
		pid:      9001,
		queryID:  ptr(ordersItemsEnd.queryid),
		usesysid: "99", // same statement shape, a different role's values
		state:    "idle",
		query:    "SELECT * FROM order_items WHERE order_id = 777777",
		ranFor:   1,
	})

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{other}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

	assert.NotContains(t, strings.Join(q.submitted, "\n"), "777777",
		"another role's literal values were submitted under this candidate's queryid")
	assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"],
		"with no session of its own the candidate falls to the normalized text")
}

func TestExplainLiteralTierRefusesAnotherDatabasesSession(t *testing.T) {
	elsewhere := activityValue(activityFixture{
		pid:     9002,
		queryID: ptr(ordersItemsEnd.queryid),
		state:   "idle",
		query:   "SELECT * FROM order_items WHERE order_id = 555",
		ranFor:  1,
	})
	elsewhere[7] = ptr("16999")

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{elsewhere}))

	e := NewExplain(ExplainModeAll, itemsRanker())

	var buf bytes.Buffer
	unidentified := explainContext(2, 2)
	unidentified.DBID = ""

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))
	require.NoError(t, e.Sample(context.Background(), q, &buf, unidentified))

	assert.NotContains(t, strings.Join(q.submitted, "\n"), "555",
		"a session in another database cannot carry this candidate's text")
}

func TestExplainNeverSubmitsTextTheAgentsOwnCapCut(t *testing.T) {
	full := "SELECT " + strings.Repeat("a", DefaultMaxQueryText) + " FROM t WHERE x = $1"
	cut := string([]rune(full)[:DefaultMaxQueryText+1])

	start, end := pg18Statement(ordersItemsStart), pg18Statement(ordersItemsEnd)
	start.query, end.query = ptr(cut), ptr(cut)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll,
		rankerWith([]statementRow{start}, []statementRow{end})), q)

	assert.Empty(t, q.submitted, "a cap+1 prefix ends mid-token and is not submittable")
	assert.Equal(t, reasonTextTruncated, blocks[0].fields["reason"],
		"and the block says which cap cut it: this one is the agent's, not the server's")
}

func TestExplainNeverSubmitsActivityTextTheAgentsOwnCapCut(t *testing.T) {
	long := "SELECT * FROM order_items WHERE note = '" +
		strings.Repeat("z", DefaultMaxQueryText) + "'"

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
		pid:        4021,
		queryID:    ptr(ordersItemsEnd.queryid),
		state:      "idle",
		query:      string([]rune(long)[:DefaultMaxQueryText+1]),
		queryBytes: 512,
		ranFor:     4.2,
	})}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

	assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"],
		"the cut activity text is refused and the normalized text carries the candidate")

	for _, statement := range q.submitted {
		assert.NotContains(t, statement, "zzz", "the agent-cut activity text was submitted")
	}
}

func TestExplainServerTruncationGateCoversTheMultibyteBand(t *testing.T) {
	for _, octets := range []int64{1020, 1021, 1022, 1023} {
		t.Run(fmt.Sprintf("octets=%d", octets), func(t *testing.T) {
			q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
			q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
				pid:        4021,
				queryID:    ptr(ordersItemsEnd.queryid),
				state:      "idle",
				query:      "SELECT * FROM order_items WHERE order_id = 4021",
				queryBytes: octets,
				ranFor:     4.2,
			})}))

			blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

			assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"],
				"text at %d octets under a 1024-byte cap may be a clipped prefix, and "+
					"a gate pinned to size-1 alone would submit it", octets)
		})
	}

	t.Run("well under the cap is still literal", func(t *testing.T) {
		blocks := runExplainSamples(t,
			NewExplain(ExplainModeAll, itemsRanker()),
			newFakeExplainConn(readableLog(t, unrelatedTraffic)))

		assert.Equal(t, planModeEstimatedLiteral, blocks[0].fields["mode"],
			"the widened band must not swallow ordinary text")
	})
}

func TestExplainLoggedPlanIsClaimedOnce(t *testing.T) {
	aStart, bStart := pg18Statement(ordersItemsStart), pg18Statement(ordersItemsStart)
	a, b := pg18Statement(ordersItemsEnd), pg18Statement(ordersItemsEnd)
	bStart.userid, b.userid = ptr("99"), ptr("99")

	q := newFakeExplainConn(readableLog(t, ""))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	e := NewExplain(ExplainModeLogged, rankerWith(
		[]statementRow{aStart, bStart}, []statementRow{a, b}))

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))
	appendFile(t, currentLogPath(t, q), itemsPlanEntry("412.5"))
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	blocks := parseTextArtifact(t, buf.String())
	require.Len(t, blocks, 3, "two candidates and the summary")

	assert.Equal(t, planModeLogged, blocks[0].fields["mode"], "the highest-ranked claimant keeps it")
	assert.Equal(t, planModeNone, blocks[1].fields["mode"])
	assert.Equal(t, reasonNoLoggedPlan, blocks[1].fields["reason"])

	summary := summaryOf(t, blocks)
	assert.Equal(t, "1", summary.fields["plans_harvested"])
	assert.Equal(t, "1", summary.fields["plans_written"],
		"one stored entry can only be written once")
	assert.Equal(t, "1", summary.fields["plans_ambiguous"])
}

func TestExplainSummaryCarriesTheLogReadsOwnState(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, ""))

	e := NewExplain(ExplainModeAll, itemsRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))

	appendFile(t, currentLogPath(t, q), strings.Repeat("x", MaxScanBytes+1)+"\n")

	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(2, 2)))

	summary := summaryOf(t, parseTextArtifact(t, buf.String()))

	assert.Equal(t, "true", summary.fields["scan_truncated"],
		"the store overran its cap and the artifact has to say so")
	assert.NotEmpty(t, summary.fields["skipped_bytes"])
	assert.Equal(t, "0", summary.fields["plans_harvested"],
		"zero beside scan_truncated= is legible; zero alone would be a measurement")
}

func TestExplainFallbackDoesNotRankIdleInTransactionAsRunning(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult([][]any{
		activityValue(activityFixture{
			pid: 4102, state: "idle in transaction",
			query:      "SELECT * FROM order_items WHERE order_id = 1",
			runningFor: 10800, ranFor: 0.002,
		}),
		activityValue(activityFixture{
			pid: 4021, state: "active",
			query:      "SELECT count(*) FROM order_items",
			runningFor: 42.5,
		}),
	}))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, explainRanker()), q)

	assert.Equal(t, "4021", blocks[0].fields["pid"],
		"a 2ms query whose session then sat idle for three hours must not outrank a "+
			"statement that is still executing")
	assert.Equal(t, "4102", blocks[1].fields["pid"])
}

func TestExplainDoesNotSubmitStatementsThatNeverRanInTheWindow(t *testing.T) {
	same := pg18Statement(ordersItemsEnd)

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(rowsResult(factsOnlyActivity()))

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll,
		rankerWith([]statementRow{same}, []statementRow{same})), q)

	assert.Empty(t, q.submitted)

	summary := summaryOf(t, blocks)
	assert.Equal(t, "1", summary.fields["excluded_no_exec_time"])
	assert.Equal(t, rankingActivityFallback, summary.fields["ranking"],
		"nothing rankable survived, which is the fallback's trigger")
}

func TestExplainRefusesAMultiStatementText(t *testing.T) {
	batch := "SET application_name = 'x'; SELECT count(*) FROM order_items"

	t.Run("literal text falls through to the normalized form", func(t *testing.T) {
		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
			pid: 4021, queryID: ptr(ordersItemsEnd.queryid), state: "idle",
			query: batch, ranFor: 4.2,
		})}))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

		require.Len(t, q.submitted, 1)
		assert.NotContains(t, q.submitted[0], "SET application_name",
			"the batch must never reach the server as one EXPLAIN")
		assert.Equal(t, planModeEstimatedGeneric, blocks[0].fields["mode"],
			"a literal text this cannot use does not end the candidate")
	})

	t.Run("with no normalized form to fall back to", func(t *testing.T) {
		start, end := pg18Statement(ordersItemsStart), pg18Statement(ordersItemsEnd)
		cut := ptr(string([]rune("SELECT " +
			strings.Repeat("a", DefaultMaxQueryText))[:DefaultMaxQueryText+1]))
		start.query, end.query = cut, cut

		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
			pid: 4021, queryID: ptr(ordersItemsEnd.queryid), state: "idle",
			query: batch, ranFor: 4.2,
		})}))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll,
			rankerWith([]statementRow{start}, []statementRow{end})), q)

		assert.Empty(t, q.submitted)
		assert.Equal(t, reasonMultiStatement, blocks[0].fields["reason"])
	})

	t.Run("and the fallback's own candidates", func(t *testing.T) {
		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
			pid: 4021, state: "active", runningFor: 12,
			query: "SELECT 1; SELECT count(*) FROM order_items",
		})}))

		blocks := runExplainSamples(t, NewExplain(ExplainModeAll, explainRanker()), q)

		assert.Empty(t, q.submitted, "the fallback reaches the simple protocol too")
		assert.Equal(t, reasonMultiStatement, blocks[0].fields["reason"])
	})

	t.Run("the allowlist refuses a batch whose first keyword is a utility", func(t *testing.T) {
		q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
		q.activity = repeat(rowsResult([][]any{activityValue(activityFixture{
			pid: 4021, state: "active", query: batch, runningFor: 12,
		})}))

		summary := summaryOf(t, runExplainSamples(t,
			NewExplain(ExplainModeAll, explainRanker()), q))

		assert.Empty(t, q.submitted)
		assert.Equal(t, "1", summary.fields["excluded_utility"],
			"two gates, and the cheaper one binds first")
	})

	t.Run("a trailing separator is not a second statement", func(t *testing.T) {
		assert.False(t, multiStatement("SELECT 1;"))
		assert.False(t, multiStatement("  SELECT 1 ;  "))
		assert.True(t, multiStatement("SELECT 1; SELECT 2"))
		assert.True(t, multiStatement("SELECT 1; SELECT 2;"))
	})
}

func TestExplainBoundsASubmittedPlan(t *testing.T) {
	line := strings.Repeat("p", 4096)
	rows := make([][]any, 0, 512)

	for range 512 {
		rows = append(rows, []any{line})
	}

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.defaultPlan = rowsResult(rows)

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

	assert.Equal(t, "true", blocks[0].fields["plan_truncated"])
	assert.LessOrEqual(t, len(blocks[0].body), MaxPlanBytes+len(line)+1,
		"the body stops at the cap rather than holding whatever the server sent")
}

func TestExplainClosingIsSilentWhenNothingWasEverAttempted(t *testing.T) {
	var buf bytes.Buffer

	e := NewExplain(ExplainModeAll, explainRanker())
	require.NoError(t, e.WriteClosing(&buf, explainContext(2, 2)))

	assert.Empty(t, buf.String(),
		"a connect failure is two lines: what was attempted, and why not")
}

func TestExplainClosingStillReportsAnArmedTailThatNeverResolved(t *testing.T) {
	q := newFakeExplainConn(deniedQuerier(logSettings{}))

	e := NewExplain(ExplainModeAll, explainRanker())

	var buf bytes.Buffer
	require.NoError(t, e.Sample(context.Background(), q, &buf, explainContext(1, 2)))
	require.NoError(t, e.WriteClosing(&buf, explainContext(2, 2)))

	summary := summaryOf(t, parseTextArtifact(t, buf.String()))
	assert.NotEmpty(t, summary.fields["log_reason"],
		"the tail was opened and did resolve to an answer, which is worth reporting")
	assert.False(t, summary.has("plans_harvested"),
		"but no count, because nothing was ever read")
}

func TestExplainReportsAFailedActivityRead(t *testing.T) {
	readErr := errors.New("permission denied for function pg_encoding_max_length")

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	q.activity = repeat(fakeResult{err: readErr})

	blocks := runExplainSamples(t, NewExplain(ExplainModeAll, itemsRanker()), q)

	summary := summaryOf(t, blocks)
	assert.Contains(t, summary.fields["activity_error"], "permission denied")
	assert.False(t, summary.has("auto_explain_visible"),
		"a fact nobody read is dropped, not defaulted")
}

func TestExplainWritesEachBlockAsOneWrite(t *testing.T) {
	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))

	e := NewExplain(ExplainModeAll, itemsRanker())

	require.NoError(t, e.Sample(context.Background(), q, &bytes.Buffer{}, explainContext(1, 2)))

	sink := &countingWriter{}
	require.NoError(t, e.Sample(context.Background(), q, sink, explainContext(2, 2)))

	assert.Equal(t, len(parseTextArtifact(t, sink.buf.String())), sink.writes,
		"one Write per block, header and body together")
}

func TestExplainReadsTheLastQueryIdentifier(t *testing.T) {
	plan := []byte(
		" Seq Scan on public.t  (cost=0.00..1.00 rows=1 width=4)\n" +
			"   Filter: (note = 'Query Identifier: 42'::text)\n" +
			" Query Identifier: -4821096637582910234\n")

	assert.Equal(t, "-4821096637582910234", planQueryIdentifier(plan),
		"a string literal in the customer's own SQL must not key the attachment")

	assert.Empty(t, planQueryIdentifier([]byte(" Seq Scan on public.t\n")))
}

func TestExplainActivityReadIsBounded(t *testing.T) {
	assert.Contains(t, activitySQL, "LIMIT $1")
	assert.Contains(t, activitySQL, "left(a.query, $2)")

	q := newFakeExplainConn(readableLog(t, unrelatedTraffic))
	require.NoError(t, NewExplain(ExplainModeAll, itemsRanker()).
		Sample(context.Background(), q, &bytes.Buffer{}, explainContext(2, 2)))

	require.NotEmpty(t, q.activityArgs)
	assert.Equal(t, []any{MaxActivitySessions, DefaultMaxQueryText + 1}, q.activityArgs[0])
}

func TestExplainReadsItsOwnOIDFromTheCatalogue(t *testing.T) {
	assert.Contains(t, activitySQL, "FROM pg_catalog.pg_roles")
	assert.NotContains(t, activitySQL, "regrole")
}
