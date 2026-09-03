package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const pg18StatementColumns = "local_blk_read_time,local_blk_write_time,minmax_stats_since," +
	"shared_blk_read_time,shared_blk_write_time,stats_since,temp_blk_read_time," +
	"temp_blk_write_time,toplevel"

func extensionRow(facts extensionFacts) []any {
	return []any{
		facts.schema,
		facts.version,
		facts.libraryLoaded,
		facts.visible,
		facts.schemaUsage,
		facts.hasInfo,
		facts.meetsMinVersion,
		facts.optionalColumns,
	}
}

func healthyExtension() extensionFacts {
	return extensionFacts{
		schema:          "public",
		version:         "1.12",
		libraryLoaded:   true,
		visible:         true,
		schemaUsage:     true,
		hasInfo:         true,
		meetsMinVersion: true,
		optionalColumns: ptr(pg18StatementColumns),
	}
}

func absentExtension() extensionFacts {
	return extensionFacts{libraryLoaded: true}
}

func pathHiddenExtension() extensionFacts {
	return extensionFacts{
		schema:        "yc_ext",
		version:       "1.12",
		libraryLoaded: true,
		schemaUsage:   true,
	}
}

func deniedExtension() extensionFacts {
	facts := pathHiddenExtension()
	facts.schemaUsage = false

	return facts
}

func shadowedExtension() extensionFacts {
	facts := healthyExtension()
	facts.visible = false
	facts.optionalColumns = nil
	facts.meetsMinVersion = false

	return facts
}

func tooOldExtension() extensionFacts {
	return extensionFacts{
		schema:          "public",
		version:         "1.7",
		libraryLoaded:   true,
		visible:         true,
		schemaUsage:     true,
		optionalColumns: ptr("blk_read_time,blk_write_time"),
	}
}

func unloadedExtension() extensionFacts {
	facts := healthyExtension()
	facts.libraryLoaded = false

	return facts
}

var testStatsSince = time.Date(2026, 8, 7, 3, 6, 21, 693*int(time.Millisecond), time.UTC)

type statementFixture struct {
	queryid  int64
	userid   string
	query    string
	calls    int64
	execTime float64
	minExec  float64
	maxExec  float64
	rows     int64
	hit      int64
	read     int64
	walRecs  int64
	walFPI   int64
	walBytes string
}

func pg18Statement(f statementFixture) statementRow {
	return statementRow{
		queryid:  ptr(f.queryid),
		userid:   ptr(f.userid),
		dbid:     ptr("16401"),
		toplevel: ptr(true),

		plans:         ptr(int64(0)),
		totalPlanTime: ptr(0.0),
		minPlanTime:   ptr(0.0),
		maxPlanTime:   ptr(0.0),

		calls:         ptr(f.calls),
		totalExecTime: ptr(f.execTime),
		minExecTime:   ptr(f.minExec),
		maxExecTime:   ptr(f.maxExec),
		rows:          ptr(f.rows),

		sharedBlksHit:     ptr(f.hit),
		sharedBlksRead:    ptr(f.read),
		sharedBlksDirtied: ptr(int64(0)),
		sharedBlksWritten: ptr(int64(0)),
		localBlksHit:      ptr(int64(0)),
		localBlksRead:     ptr(int64(0)),
		localBlksDirtied:  ptr(int64(0)),
		localBlksWritten:  ptr(int64(0)),
		tempBlksRead:      ptr(int64(0)),
		tempBlksWritten:   ptr(int64(0)),

		sharedBlkReadTime:  ptr(0.0),
		sharedBlkWriteTime: ptr(0.0),
		localBlkReadTime:   ptr(0.0),
		localBlkWriteTime:  ptr(0.0),
		tempBlkReadTime:    ptr(0.0),
		tempBlkWriteTime:   ptr(0.0),

		walRecords: ptr(f.walRecs),
		walFPI:     ptr(f.walFPI),
		walBytes:   ptr(f.walBytes),

		statsSince:       &testStatsSince,
		minmaxStatsSince: &testStatsSince,

		query: ptr(f.query),
	}
}

func pre17Statement(f statementFixture) statementRow {
	row := pg18Statement(f)

	row.blkReadTime, row.blkWriteTime = ptr(0.0), ptr(0.0)
	row.sharedBlkReadTime, row.sharedBlkWriteTime = nil, nil
	row.localBlkReadTime, row.localBlkWriteTime = nil, nil
	row.statsSince, row.minmaxStatsSince = nil, nil

	return row
}

func maskedStatement(f statementFixture) statementRow {
	row := pg18Statement(f)

	row.queryid = nil
	row.query = ptr(insufficientPrivilege)

	return row
}

var (
	ordersUpdateStart = statementFixture{
		queryid: -7710234988821003345, userid: "10",
		query: "UPDATE orders SET status = $1 WHERE id = $2",
		calls: 55201, execTime: 18820045.2, minExec: 1.204, maxExec: 9902.1,
		rows: 55201, hit: 1204884, read: 8821,
		walRecs: 55201, walFPI: 412, walBytes: "4980112",
	}
	ordersUpdateEnd = statementFixture{
		queryid: -7710234988821003345, userid: "10",
		query: "UPDATE orders SET status = $1 WHERE id = $2",
		calls: 55263, execTime: 18859725.2, minExec: 1.204, maxExec: 9902.1,
		rows: 55263, hit: 1205140, read: 8829,
		walRecs: 55263, walFPI: 415, walBytes: "4985340",
	}

	ordersItemsStart = statementFixture{
		queryid: -4821096637582910234, userid: "10",
		query: "SELECT * FROM order_items WHERE order_id = $1",
		calls: 128400, execTime: 9820410.5, minExec: 0.812, maxExec: 4120.4,
		rows: 128412, hit: 4021884, read: 398210, walBytes: "0",
	}
	ordersItemsEnd = statementFixture{
		queryid: -4821096637582910234, userid: "10",
		query: "SELECT * FROM order_items WHERE order_id = $1",
		calls: 128740, execTime: 9891810.5, minExec: 0.812, maxExec: 4120.4,
		rows: 128752, hit: 4025901, read: 398340, walBytes: "0",
	}

	agentRead = statementFixture{
		queryid: 1104882300112034551, userid: "16385",
		query: "WITH m AS MATERIALIZED ( SELECT s.queryid, s.userid::text AS userid, " +
			"s.dbid::text AS dbid, ...",
		calls: 1, execTime: 4.118, minExec: 4.118, maxExec: 4.118,
		rows: 3, hit: 412, walBytes: "0",
	}

	ordersInventoryStart = statementFixture{
		queryid: 5548219003471002234, userid: "10",
		query: "SELECT * FROM inventory WHERE sku = $1",
		calls: 884210, execTime: 42011200.8, minExec: 0.019, maxExec: 88.2,
		rows: 884210, hit: 8821040, read: 12048, walBytes: "0",
	}
	ordersInventoryEnd = statementFixture{
		queryid: 5548219003471002234, userid: "10",
		query: "SELECT * FROM inventory WHERE sku = $1",
		calls: 884328, execTime: 42021226.8, minExec: 0.019, maxExec: 88.2,
		rows: 884328, hit: 8821994, read: 12053, walBytes: "0",
	}
)

func statementValues(rows []statementRow, total int64) [][]any {
	values := make([][]any, len(rows))

	for i := range rows {
		row := make([]any, 0, len(statementColumnSpecs)+1)

		for _, dest := range rows[i].dest() {
			row = append(row, reflect.ValueOf(dest).Elem().Interface())
		}

		values[i] = append(row, total)
	}

	return values
}

var testInfoStatsReset = time.Date(2026, 8, 7, 3, 6, 21, 693*int(time.Millisecond), time.UTC)

type fakeSlowQueriesConn struct {
	*fakeWindowConn

	extension  fakeRow
	info       fakeRow
	statements []fakeResult

	sql            []string
	extensionArgs  [][]any
	statementsArgs [][]any
	deadlines      []time.Time
}

func newFakeSlowQueriesConn(facts extensionFacts) *fakeSlowQueriesConn {
	return &fakeSlowQueriesConn{
		fakeWindowConn: newFakeWindowConn(),
		extension:      fakeRow{values: extensionRow(facts)},
		info:           fakeRow{values: []any{ptr(int64(0)), &testInfoStatsReset}},
		statements:     repeat(rowsResult(nil)),
	}
}

func (c *fakeSlowQueriesConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	switch sql {
	case extensionSQL:
		c.record(ctx, sql)
		c.extensionArgs = append(c.extensionArgs, args)

		return c.extension

	case infoSQL:
		c.record(ctx, sql)

		return c.info
	}

	return c.fakeWindowConn.QueryRow(ctx, sql, args...)
}

func (c *fakeSlowQueriesConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != statementsSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	c.record(ctx, sql)
	c.statementsArgs = append(c.statementsArgs, args)

	return answer(&c.statements)
}

func (c *fakeSlowQueriesConn) record(ctx context.Context, sql string) {
	c.sql = append(c.sql, sql)

	deadline, _ := ctx.Deadline()
	c.deadlines = append(c.deadlines, deadline)
}

func slowQueriesSampleContext() SampleContext {
	return SampleContext{
		At: at(32, 5, 61), Index: 1, Total: 2,
		Database: "orders_db", DBID: "16401",
	}
}

func takeSlowQueriesSample(t *testing.T, conn *fakeSlowQueriesConn, s SampleContext) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, NewSlowQueries().Sample(context.Background(), conn, &buf, s),
		"a read that could not be classified is still a written sample; only a failed write is an error")

	return buf.String()
}

func splitBlocks(sample string) (headers, body []string) {
	for line := range strings.SplitSeq(strings.TrimSuffix(sample, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			headers = append(headers, line)

			continue
		}

		body = append(body, line)
	}

	return headers, body
}

func statementBody(sample string) []string {
	var (
		rows []string
		in   bool
	)

	header := strings.Join(statementColumns, ",")

	for line := range strings.SplitSeq(strings.TrimSuffix(sample, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			in = strings.Contains(line, "source=pg_stat_statements v=")

		case in && line != header:
			rows = append(rows, line)
		}
	}

	return rows
}

func TestSlowQueriesColumnSpecsDriveTheDerivedLists(t *testing.T) {
	optional := 0

	for _, spec := range statementColumnSpecs {
		if spec.optional {
			optional++
		}
	}

	assert.Len(t, statementColumnSpecs, 37, "the widest block in the feature, where only eight were asked for")
	assert.Equal(t, 11, optional, "the columns pg_stat_statements gained or lost after extension 1.8")

	names := make([]string, len(statementColumnSpecs))
	for i, spec := range statementColumnSpecs {
		names[i] = spec.name
	}

	assert.Equal(t, names, statementColumns,
		"the CSV header is the column list in order, so it cannot drift from the select list built beside it")
	assert.Len(t, optionalStatementColumns, optional)
	assert.Equal(t, "queryid", statementColumns[0], "the first component of the merge key leads")
	assert.Equal(t, "query", statementColumns[len(statementColumns)-1], "the only unbounded column closes")
	assert.Equal(t, []string{"dealloc", "stats_reset"}, infoColumns)

	cells, _ := statementCells([]statementRow{pg18Statement(ordersItemsStart)})
	require.Len(t, cells, 1)
	assert.Len(t, cells[0], 37, "the render pass agrees with the header it is written under")

	var row statementRow
	assert.Len(t, row.dest(), 37, "and so does the scan")
}

func TestSlowQueriesStatementIsOneJsonbPerRow(t *testing.T) {
	assert.Equal(t, 1, strings.Count(statementsSQL, "to_jsonb("),
		"eleven inline extractions build eleven jsonb documents per row: 34.2 ms against 4.7 ms "+
			"on ~290 rows, ~590 ms against ~80 ms on a saturated view, and nothing fails when it regresses")

	assert.Contains(t, statementsSQL, "AS MATERIALIZED",
		"not what buys the 7x - count(*) OVER () does - but the declared insurance that keeps "+
			"single evaluation true if that window function ever moves out of the CTE")

	assert.Contains(t, statementsSQL, "to_jsonb(s) - 'query'",
		"the one field neither cap bounds, kept out of the tuplestore so a spill cannot land in "+
			"pg_health.txt's temp_files as customer I/O")
}

func TestSlowQueriesStatementTakesNoRanking(t *testing.T) {
	_, orderBy, found := strings.Cut(statementsSQL, "ORDER BY")
	require.True(t, found)

	assert.NotContains(t, orderBy, "total_exec_time")
	assert.NotContains(t, orderBy, "calls")
	assert.NotContains(t, orderBy, "DESC")

	for _, key := range statementKeyColumns {
		assert.Contains(t, orderBy, key, "the sort key is the view's own identity")
	}
}

func TestSlowQueriesStatementBoundsWhatItReads(t *testing.T) {
	cte, outer, found := strings.Cut(statementsSQL, "\nSELECT ")
	require.True(t, found)

	assert.Contains(t, cte, "count(*) OVER ()",
		"inside the CTE, so it is evaluated before the LIMIT and is the uncapped total")
	assert.NotContains(t, outer, "count(*) OVER ()")

	assert.Contains(t, cte, "left(s.query, $2)",
		"the only bound on this column: pg_stat_statements keeps its text in an external file "+
			"and does not honour track_activity_query_size")
	assert.Contains(t, outer, "LIMIT $1")

	assert.Contains(t, cte, "s.wal_bytes::text", "numeric is unbounded where int64 is not")
	assert.Contains(t, cte, "s.userid::text")
	assert.Contains(t, cte, "s.dbid::text")

	for _, spec := range statementColumnSpecs {
		assert.Contains(t, outer, spec.expr, spec.name)
	}
}

func TestSlowQueriesMaskedRowsScan(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = repeat(rowsResult(statementValues([]statementRow{
		maskedStatement(ordersItemsStart),
	}, 1)))

	sample := takeSlowQueriesSample(t, conn, slowQueriesSampleContext())

	headers, _ := splitBlocks(sample)
	assert.NotContains(t, headers[1], "error=")
	assert.Contains(t, headers[1], "statements_written=1 statements_total=1 truncated=false")

	body := statementBody(sample)
	require.Len(t, body, 1)
	row := body[0]

	assert.True(t, strings.HasPrefix(row, ","),
		"an empty lead cell begins the line with a comma - which pg_sessions.txt's rows never do")
	assert.False(t, strings.HasPrefix(row, "#"),
		"and that is the claim the parse contract actually needs")
	assert.Contains(t, row, insufficientPrivilege,
		"captured verbatim and never matched on: pg_metadata.txt's has_pg_read_all_stats is what "+
			"tells a masked capture from an unmasked one")
	assert.Contains(t, row, ",128400,", "the counters are exact, which is what makes this worse than an error")
}

func TestSlowQueriesCaps(t *testing.T) {
	rows := []statementRow{
		pg18Statement(ordersUpdateStart),
		pg18Statement(ordersItemsStart),
		pg18Statement(ordersInventoryStart),
	}

	t.Run("under the cap", func(t *testing.T) {
		conn := newFakeSlowQueriesConn(healthyExtension())
		conn.statements = repeat(rowsResult(statementValues(rows, 3)))

		headers, _ := splitBlocks(takeSlowQueriesSample(t, conn, slowQueriesSampleContext()))
		assert.Contains(t, headers[1], "statements_written=3 statements_total=3 truncated=false")
		assert.NotContains(t, headers[1], "queries_truncated=")
	})

	t.Run("cap binds", func(t *testing.T) {
		conn := newFakeSlowQueriesConn(healthyExtension())
		conn.statements = repeat(rowsResult(statementValues(rows[:2], 9412)))

		var buf bytes.Buffer
		capped := &SlowQueries{MaxStatements: 2}
		require.NoError(t, capped.Sample(context.Background(), conn, &buf,
			slowQueriesSampleContext()))

		headers, _ := splitBlocks(buf.String())
		assert.Contains(t, headers[1], "statements_written=2 statements_total=9412 truncated=true")
		assert.Equal(t, []any{2, DefaultMaxQueryText + 1}, conn.statementsArgs[0])
	})

	t.Run("query past the text cap", func(t *testing.T) {
		long := ordersItemsStart
		long.query = strings.Repeat("é", DefaultMaxQueryText+1)

		at := ordersInventoryStart
		at.query = strings.Repeat("x", DefaultMaxQueryText)

		conn := newFakeSlowQueriesConn(healthyExtension())
		conn.statements = repeat(rowsResult(statementValues([]statementRow{
			pg18Statement(long), pg18Statement(at),
		}, 2)))

		sample := takeSlowQueriesSample(t, conn, slowQueriesSampleContext())

		headers, _ := splitBlocks(sample)
		assert.Contains(t, headers[1], "queries_truncated=1",
			"one past the cap and one exactly at it; the mark counts runes, so a multi-byte "+
				"character is never split in half")

		body := statementBody(sample)
		require.Len(t, body, 2)
		assert.Contains(t, body[0], strings.Repeat("é", DefaultMaxQueryText)+"...")
		assert.NotContains(t, body[1], "...")
	})
}

func TestSlowQueriesFailedReadCostsItsOwnBlock(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = repeat(errResult(errors.New("canceling statement due to statement timeout (SQLSTATE 57014)")))

	writer := &countingWriter{}
	require.NoError(t, NewSlowQueries().Sample(context.Background(), conn, writer, slowQueriesSampleContext()))

	headers, body := splitBlocks(writer.buf.String())

	assert.Contains(t, headers[1], "57014")
	assert.Contains(t, headers[1], "optional_columns="+pg18StatementColumns,
		"a fact the preflight established about the server, not about the read")
	assert.NotContains(t, headers[1], "statements_total=",
		"statements_total=0 would assert the server has no query statistics, where the truth "+
			"is that nobody could count them")
	assert.NotContains(t, headers[0], "error=", "the info block beside it is untouched")
	assert.Len(t, body, 3, "the info block keeps its row; the statements block is header-only")
	assert.Empty(t, statementBody(writer.buf.String()))
	assert.Equal(t, 1, writer.writes)
}

func TestSlowQueriesAbsencesAreReasonsNotErrors(t *testing.T) {
	cases := []struct {
		name     string
		facts    extensionFacts
		reason   string
		wantKeys string
	}{
		{
			"absent", absentExtension(), reasonExtensionAbsent,
			"extension_version= extension_schema= library_loaded=true reason=extension_absent",
		},
		{
			"path", pathHiddenExtension(), reasonNotInSearchPath,
			"extension_version=1.12 extension_schema=yc_ext library_loaded=true " +
				"schema_usage=true reason=not_in_search_path",
		},
		{
			"no usage", deniedExtension(), reasonNotInSearchPath,
			"extension_version=1.12 extension_schema=yc_ext library_loaded=true " +
				"schema_usage=false reason=not_in_search_path",
		},
		{
			"shadowed", shadowedExtension(), reasonNotInSearchPath,
			"extension_version=1.12 extension_schema=public library_loaded=true " +
				"schema_usage=true reason=not_in_search_path",
		},
		{
			"too old", tooOldExtension(), reasonExtensionTooOld,
			"extension_version=1.7 extension_schema=public library_loaded=true reason=extension_too_old",
		},
		{
			"unloaded", unloadedExtension(), reasonLibraryNotLoaded,
			"extension_version=1.12 extension_schema=public library_loaded=false reason=library_not_loaded",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sample := takeSlowQueriesSample(t, newFakeSlowQueriesConn(tc.facts), slowQueriesSampleContext())

			headers, body := splitBlocks(sample)

			require.Len(t, headers, 2, "both blocks are written whatever the preflight said")

			for _, header := range headers {
				assert.Contains(t, header, "reason="+tc.reason)
				assert.NotContains(t, header, "error=",
					"an error key means a read failed, and none of these is a read that failed")
				assert.NotContains(t, header, "optional_columns=",
					"with no rows there are no cells for the presence set to disambiguate")
			}

			assert.Contains(t, headers[0], "source=pg_stat_statements_info")
			assert.Contains(t, headers[1], "source=pg_stat_statements")
			assert.Contains(t, headers[1], tc.wantKeys)

			assert.Equal(t, []string{
				strings.Join(infoColumns, ","),
				strings.Join(statementColumns, ","),
			}, body, "header-only: the column contract is written even with no rows")

			assert.NotEmpty(t, sample,
				"a zero-byte artifact is dropped on the way out, and this file is the finding")
		})
	}
}

func TestSlowQueriesClassifiesANullOptionalColumnsProbe(t *testing.T) {
	facts := absentExtension()
	require.Nil(t, facts.optionalColumns)

	sample := takeSlowQueriesSample(t, newFakeSlowQueriesConn(facts), slowQueriesSampleContext())

	headers, _ := splitBlocks(sample)
	for _, header := range headers {
		assert.Contains(t, header, "reason="+reasonExtensionAbsent)
		assert.NotContains(t, header, "error=")
	}
}

func TestSlowQueriesWritesLibraryLoadedOnEveryCapture(t *testing.T) {
	healthy := takeSlowQueriesSample(t, newFakeSlowQueriesConn(healthyExtension()), slowQueriesSampleContext())

	headers, _ := splitBlocks(healthy)
	assert.Contains(t, headers[1], "library_loaded=true")
	assert.Contains(t, headers[1], "extension_version=1.12 extension_schema=public")
	assert.Contains(t, headers[1], "optional_columns="+pg18StatementColumns,
		"which of the eleven the server had, so an empty cell can be told from an absent column")
	assert.NotContains(t, headers[1], "reason=")
	assert.NotContains(t, headers[0], "reason=")

	combined := absentExtension()
	combined.libraryLoaded = false

	headers, _ = splitBlocks(takeSlowQueriesSample(t, newFakeSlowQueriesConn(combined), slowQueriesSampleContext()))
	assert.Contains(t, headers[1], "library_loaded=false")
	assert.Contains(t, headers[1], "reason="+reasonExtensionAbsent,
		"CREATE EXTENSION is what has to happen first, so it is the reason and the library is the key")
}

func TestSlowQueriesFailedPreflightIsAnErrorOnBothBlocks(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.extension = fakeRow{err: errors.New("canceling statement due to statement timeout (SQLSTATE 57014)")}

	sample := takeSlowQueriesSample(t, conn, slowQueriesSampleContext())

	headers, body := splitBlocks(sample)
	require.Len(t, headers, 2)

	for _, header := range headers {
		assert.Contains(t, header, "error=")
		assert.Contains(t, header, "57014")
		assert.NotContains(t, header, "reason=",
			"a failed preflight classified nothing, and naming one of the four would be a guess")
		assert.NotContains(t, header, "library_loaded=")
		assert.NotContains(t, header, "extension_version=")
	}

	assert.Len(t, body, 2, "header-only, as the four reasons are")
}

func TestSlowQueriesBracketsTheWindowWithTheInfoBlock(t *testing.T) {
	opening := slowQueriesSampleContext()

	conn := newFakeSlowQueriesConn(healthyExtension())
	headers, _ := splitBlocks(takeSlowQueriesSample(t, conn, opening))

	assert.Contains(t, headers[0], "source=pg_stat_statements_info", "the info block leads the opening sample")
	assert.Contains(t, headers[1], "source=pg_stat_statements v=")
	assert.Equal(t, []string{extensionSQL, infoSQL, statementsSQL}, conn.sql,
		"and the read really did run first, which is the half the block order alone cannot show")

	closing := opening
	closing.Index = 2

	conn = newFakeSlowQueriesConn(healthyExtension())
	headers, _ = splitBlocks(takeSlowQueriesSample(t, conn, closing))

	assert.Contains(t, headers[0], "source=pg_stat_statements v=", "and closes the last one")
	assert.Contains(t, headers[1], "source=pg_stat_statements_info")
	assert.Equal(t, []string{extensionSQL, statementsSQL, infoSQL}, conn.sql,
		"so the two stats_reset readings enclose every other read in the window")

	degenerate := opening
	degenerate.Total = 1

	headers, _ = splitBlocks(takeSlowQueriesSample(t, newFakeSlowQueriesConn(healthyExtension()), degenerate))

	assert.Contains(t, headers[1], "source=pg_stat_statements_info",
		"a one-sample window is both endpoints at once and takes the closing order")
}

func TestSlowQueriesInfoViewAbsentIsScopedToOneVersion(t *testing.T) {
	meetsMinVersion := healthyExtension()
	meetsMinVersion.version = "1.8"
	meetsMinVersion.hasInfo = false
	meetsMinVersion.optionalColumns = ptr("blk_read_time,blk_write_time")

	conn := newFakeSlowQueriesConn(meetsMinVersion)
	headers, body := splitBlocks(takeSlowQueriesSample(t, conn, slowQueriesSampleContext()))

	assert.Contains(t, headers[0], "extension_version=1.8 reason="+reasonViewAbsent)
	assert.NotContains(t, headers[0], "error=", "an unwritten view is not a read that failed")
	assert.NotContains(t, headers[1], "reason=", "the statements block is fine at the floor and reads normally")
	assert.Contains(t, headers[1], "statements_written=", "1.8 is a supported extension, not a refused one")
	assert.Equal(t, []string{extensionSQL, statementsSQL}, conn.sql, "and the info read is never attempted")

	assert.Equal(t, strings.Join(infoColumns, ","), body[0], "header-only, column contract intact")

	for _, facts := range []extensionFacts{
		absentExtension(), pathHiddenExtension(), tooOldExtension(), unloadedExtension(),
	} {
		headers, _ = splitBlocks(takeSlowQueriesSample(t, newFakeSlowQueriesConn(facts), slowQueriesSampleContext()))

		assert.NotContains(t, headers[0], reasonViewAbsent,
			"view_absent beside %s would read as a second, unrelated problem", facts.reason())
		assert.Contains(t, headers[0], "reason="+facts.reason())
	}
}

func TestSlowQueriesFailedInfoReadCostsItsOwnBlock(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.info = fakeRow{err: errors.New("canceling statement due to statement timeout (SQLSTATE 57014)")}
	conn.statements = repeat(rowsResult(statementValues([]statementRow{
		pg18Statement(ordersItemsStart),
	}, 1)))

	writer := &countingWriter{}
	require.NoError(t, NewSlowQueries().Sample(context.Background(), conn, writer, slowQueriesSampleContext()))

	headers, body := splitBlocks(writer.buf.String())

	assert.Contains(t, headers[0], "57014")
	assert.NotContains(t, headers[0], "reason=")
	assert.NotContains(t, headers[1], "error=", "the statements block beside it is intact")
	assert.Contains(t, headers[1], "statements_written=1")

	assert.Len(t, body, 3, "the info block is header-only, the statements block keeps its row")
	assert.Equal(t, 1, writer.writes)
}

func TestSlowQueriesInfoRowScans(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.info = fakeRow{values: []any{ptr(int64(1204)), &testInfoStatsReset}}

	_, body := splitBlocks(takeSlowQueriesSample(t, conn, slowQueriesSampleContext()))

	assert.Equal(t, "1204,2026-08-07T03:06:21.693Z", body[1],
		"dealloc rising between the endpoints means some deltas are floors rather than values")

	conn = newFakeSlowQueriesConn(healthyExtension())
	conn.info = fakeRow{values: []any{ptr(int64(0)), nil}}

	_, body = splitBlocks(takeSlowQueriesSample(t, conn, slowQueriesSampleContext()))

	assert.Equal(t, "0,", body[1], "a never-reset cluster is an empty cell, not a failure")
}

func TestSlowQueriesWritesTheSampleInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, NewSlowQueries().Sample(context.Background(),
		newFakeSlowQueriesConn(healthyExtension()), writer, slowQueriesSampleContext()))

	assert.Equal(t, 1, writer.writes,
		"a write failing between the two blocks would leave the window's stub behind a half-written sample")
}

func TestSlowQueriesPreflightPassesTheOptionalColumnList(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())

	takeSlowQueriesSample(t, conn, slowQueriesSampleContext())

	require.Len(t, conn.extensionArgs, 1,
		"one preflight per sample, its outcome passed to both blocks: two reads could disagree "+
			"about what the server has, and the two blocks of one sample must not")
	assert.Equal(t, []any{optionalStatementColumns}, conn.extensionArgs[0])

	require.Len(t, conn.statementsArgs, 1)
	assert.Equal(t, []any{DefaultMaxStatements, DefaultMaxQueryText + 1}, conn.statementsArgs[0],
		"$2 is one rune past the cap, so the render pass can still tell a cell the agent cut "+
			"from one that arrived exactly at the limit")

	require.Len(t, conn.deadlines, 3, "the preflight and both reads")
	for _, deadline := range conn.deadlines {
		assert.False(t, deadline.IsZero(), "every read runs under StatementTimeout")
	}
}

func TestSlowQueriesReadsNothingBehindAReason(t *testing.T) {
	for _, facts := range []extensionFacts{
		absentExtension(), pathHiddenExtension(), tooOldExtension(), unloadedExtension(),
	} {
		conn := newFakeSlowQueriesConn(facts)

		takeSlowQueriesSample(t, conn, slowQueriesSampleContext())

		assert.Equal(t, []string{extensionSQL}, conn.sql, facts.reason())
	}
}

func TestSlowQueriesArtifact(t *testing.T) {
	artifact := NewSlowQueries().Artifact()

	assert.Equal(t, "pg_slow_queries", artifact.Name)
	assert.Equal(t, "pg_slow_queries.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"the statistics are cluster-wide and tagged by dbid, though the extension is installed per database")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), (&SlowQueries{Interval: 15 * time.Second}).Artifact().Schedule,
		"the run's cadence, with the close as the last sample")
	assert.Equal(t, 3*StatementTimeout, artifact.SampleBudget,
		"a preflight and two reads, which moduleDeadline sums against Capacity and Bloat")
}

func slowQueriesGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 61),
		at(32, 5, 61),
		at(32, 11, 20),
		at(32, 11, 84),
	)
}

func runSlowQueriesWindow(t *testing.T, clock *scriptedClock, conn windowConn) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   6 * time.Second,
		Collectors: []Collector{NewSlowQueries()},
		now:        clock.now,
		after:      clock.after,
		connect:    connectTo(conn),
	}

	return window.Run(context.Background())
}

func TestSlowQueriesGoldenExtensionAbsent(t *testing.T) {
	results := runSlowQueriesWindow(t, slowQueriesGoldenClock(t),
		newFakeSlowQueriesConn(absentExtension()))

	require.Equal(t, StatusComplete, results[0].Status,
		"an extension nobody created is a finding, not a failure: two samples, no error anywhere")
	assert.Equal(t, 2, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_slow_queries_extension_absent.txt"), artifactText(t, results[0]))
}

func TestSlowQueriesGoldenFull(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = queue(
		rowsResult(statementValues([]statementRow{
			pg18Statement(ordersUpdateStart),
			pg18Statement(ordersItemsStart),
			pg18Statement(ordersInventoryStart),
		}, 3)),
		rowsResult(statementValues([]statementRow{
			pg18Statement(ordersUpdateEnd),
			pg18Statement(ordersItemsEnd),
			pg18Statement(agentRead),
			pg18Statement(ordersInventoryEnd),
		}, 4)),
	)

	results := runSlowQueriesWindow(t, slowQueriesGoldenClock(t), conn)

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_slow_queries_full.txt"), artifactText(t, results[0]))
}

func TestSlowQueriesGoldenPre17(t *testing.T) {
	facts := healthyExtension()
	facts.version = "1.10"
	facts.optionalColumns = ptr("blk_read_time,blk_write_time,temp_blk_read_time,temp_blk_write_time,toplevel")

	conn := newFakeSlowQueriesConn(facts)
	conn.statements = queue(
		rowsResult(statementValues([]statementRow{
			pre17Statement(ordersUpdateStart),
			pre17Statement(ordersItemsStart),
			pre17Statement(ordersInventoryStart),
		}, 3)),
		rowsResult(statementValues([]statementRow{
			pre17Statement(ordersUpdateEnd),
			pre17Statement(ordersItemsEnd),
			pre17Statement(ordersInventoryEnd),
		}, 3)),
	)

	results := runSlowQueriesWindow(t, slowQueriesGoldenClock(t), conn)

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_slow_queries_pre17.txt"), artifactText(t, results[0]))
}

func TestSlowQueriesGoldenLeastPrivilege(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = queue(
		rowsResult(statementValues([]statementRow{
			maskedStatement(ordersUpdateStart),
			maskedStatement(ordersItemsStart),
			maskedStatement(ordersInventoryStart),
		}, 3)),
		rowsResult(statementValues([]statementRow{
			pg18Statement(agentRead),
			maskedStatement(ordersUpdateEnd),
			maskedStatement(ordersItemsEnd),
			maskedStatement(ordersInventoryEnd),
		}, 4)),
	)

	results := runSlowQueriesWindow(t, slowQueriesGoldenClock(t), conn)

	require.Equal(t, StatusComplete, results[0].Status,
		"nothing in this artifact says the capture was degraded; pg_metadata.txt's "+
			"has_pg_read_all_stats is the whole of the discriminator")
	assert.Equal(t, bloatGolden(t, "pg_slow_queries_least_privilege.txt"), artifactText(t, results[0]))
}

func TestSlowQueriesGoldenQueryText(t *testing.T) {
	multiline := ordersUpdateStart
	multiline.query = "SELECT id,\n       status\n  FROM orders\n WHERE id = $1"

	hashLine := ordersItemsStart
	hashLine.query = "SELECT 1\n# not a block header\nFROM orders WHERE id = $1"

	quoted := ordersInventoryStart
	quoted.query = `SELECT "order,id", count(*) FROM "orders,archive" WHERE note = 'a "quoted" value, with a comma'`

	multibyte := agentRead
	multibyte.queryid = 2204882300112034551
	multibyte.query = "SELECT * FROM 注文 WHERE 状態 = $1 -- ␡ ünïcödé"

	discarded := statementFixture{
		queryid: 3304882300112034551, userid: "10",
		calls: 4, execTime: 12.5, minExec: 2.1, maxExec: 4.9, rows: 4, hit: 88, walBytes: "0",
	}

	lost := pg18Statement(discarded)
	lost.query = nil

	rows := []statementRow{
		pg18Statement(multiline),
		pg18Statement(hashLine),
		pg18Statement(multibyte),
		lost,
		pg18Statement(quoted),
		maskedStatement(ordersUpdateEnd),
	}

	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = repeat(rowsResult(statementValues(rows, int64(len(rows)))))

	results := runSlowQueriesWindow(t, slowQueriesGoldenClock(t), conn)

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_slow_queries_query_text.txt"), artifactText(t, results[0]))

	headers, body := splitBlocks(artifactText(t, results[0]))

	assert.Len(t, headers, 6, "preamble, two blocks per sample, closing")
	assert.Len(t, body, 2*(3+len(rows)),
		"per sample: the info block's column header and its one row, the statements column header, "+
			"and one physical line per statement. A newline surviving into a cell would split a "+
			"row in two, and a line-oriented reader would see a record the CSV reader does not")

	for _, line := range body {
		assert.False(t, strings.HasPrefix(line, "#"),
			"no line but a block header begins with '#': the lead cell is an integer or empty, "+
				"and the query text is the last column")
	}
}

// --- endpoint retention, which pg_explain.txt ranks (never written here) -----

func TestSlowQueriesOffersEverySampleToExplainOnce(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = queue(
		rowsResult(statementValues([]statementRow{pg18Statement(ordersItemsStart)}, 1)),
		rowsResult(statementValues([]statementRow{pg18Statement(ordersInventoryStart)}, 1)),
		rowsResult(statementValues([]statementRow{pg18Statement(ordersItemsEnd)}, 1)),
	)

	sq := NewSlowQueries()

	samples := make([]string, 0, 3)

	for index := 1; index <= 3; index++ {
		s := slowQueriesSampleContext()
		s.Index, s.Total = index, 3

		var buf bytes.Buffer
		require.NoError(t, sq.Sample(context.Background(), conn, &buf, s))

		samples = append(samples, buf.String())

		feed, ok := sq.feed(s)
		require.True(t, ok, "sample %d is offered to the tick that read it", index)
		assert.Equal(t, index, feed.sample)
		assert.True(t, feed.read)
		assert.False(t, feed.truncated)
		require.Len(t, feed.rows, 1)
		assert.NotNil(t, feed.rows[0].query,
			"the feed keeps its text: it is the only place the generic mode's normalized "+
				"statement exists")

		_, again := sq.feed(s)
		assert.False(t, again, "and taken once")
	}

	assert.Less(t, strings.Index(samples[0], "source=pg_stat_statements_info"),
		strings.Index(samples[0], "source=pg_stat_statements "),
		"the opening sample reads info first")

	for _, index := range []int{1, 2} {
		assert.Greater(t, strings.Index(samples[index], "source=pg_stat_statements_info"),
			strings.Index(samples[index], "source=pg_stat_statements "),
			"sample %d reads info last: a middle sample takes the closing order, so the "+
				"first and last info readings still enclose everything between them", index+1)
	}

	assert.Contains(t, samples[1], ordersInventoryStart.query,
		"the middle sample is written in full, and offered like any other: with no "+
			"delta to compute, it is as good a feed as an endpoint")
}

func TestSlowQueriesFeedIsOfferedToItsOwnSampleOnly(t *testing.T) {
	sq := NewSlowQueries()
	sq.retain(SampleContext{Index: 2}, []statementRow{pg18Statement(ordersItemsEnd)}, false)

	_, ok := sq.feed(SampleContext{Index: 1})
	assert.False(t, ok, "another sample's read is not this sample's")

	feed, ok := sq.feed(SampleContext{Index: 2})
	require.True(t, ok)
	assert.Equal(t, 2, feed.sample)
}

func TestSlowQueriesOffersTheReasonWhenThereWereNoRows(t *testing.T) {
	t.Run("the read failed", func(t *testing.T) {
		conn := newFakeSlowQueriesConn(healthyExtension())
		conn.statements = repeat(fakeResult{err: errors.New("ERROR: permission denied")})

		sq := NewSlowQueries()

		s := slowQueriesSampleContext()
		s.Index, s.Total = 1, 2

		require.NoError(t, sq.Sample(context.Background(), conn, &bytes.Buffer{}, s))

		feed, ok := sq.feed(s)
		require.True(t, ok, "offered, so Explain can say why rather than report an idle database")
		assert.False(t, feed.read, "an unread sample must not read as an empty one: every row would look new")
		assert.Empty(t, feed.rows)
		assert.Contains(t, feed.reason, "permission denied")
	})

	t.Run("the extension is absent", func(t *testing.T) {
		conn := newFakeSlowQueriesConn(absentExtension())

		sq := NewSlowQueries()

		s := slowQueriesSampleContext()
		s.Index, s.Total = 1, 2

		require.NoError(t, sq.Sample(context.Background(), conn, &bytes.Buffer{}, s))

		feed, ok := sq.feed(s)
		require.True(t, ok)
		assert.False(t, feed.read)
		assert.Equal(t, reasonExtensionAbsent, feed.reason, "the extension's own reason, verbatim")
	})
}

func TestSlowQueriesOneTickWindowOffersItsOnlySample(t *testing.T) {
	conn := newFakeSlowQueriesConn(healthyExtension())
	conn.statements = repeat(rowsResult(statementValues([]statementRow{
		pg18Statement(ordersItemsEnd),
	}, 1)))

	sq := NewSlowQueries()

	s := slowQueriesSampleContext()
	s.Index, s.Total = 1, 1

	require.NoError(t, sq.Sample(context.Background(), conn, &bytes.Buffer{}, s))

	feed, ok := sq.feed(s)
	require.True(t, ok)
	assert.True(t, feed.read)
	require.Len(t, feed.rows, 1)
}

func TestStatementKeyTreatsNullToplevelAsAValue(t *testing.T) {
	withNull := pg18Statement(ordersItemsStart)
	withNull.toplevel = nil

	nullKey, ok := statementRowKey(withNull)
	require.True(t, ok)

	trueKey, ok := statementRowKey(pg18Statement(ordersItemsStart))
	require.True(t, ok)

	assert.NotEqual(t, trueKey, nullKey,
		"below extension 1.9 every row is NULL here; treating it as a wildcard would "+
			"merge rows the server's own key keeps apart")
	assert.Empty(t, nullKey.toplevel)
}
