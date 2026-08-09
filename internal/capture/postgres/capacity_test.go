package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colCheckpointsTimed = iota
	colCheckpointsReq
	colBuffersCheckpoint
	colBuffersClean
	colBuffersBackend
	colCheckpointerReset
	colBgwriterReset
)

const (
	colApplicationName = iota
	colBackendType
	colActiveConnections
)

var testBgwriterReset = time.Date(2026, 8, 1, 9, 15, 0, 0, time.UTC)

func answerRow(pending *[]fakeRow) pgx.Row {
	if len(*pending) == 0 {
		return fakeRow{err: errors.New("no scripted row")}
	}

	head := (*pending)[0]
	if len(*pending) > 1 {
		*pending = (*pending)[1:]
	}

	return head
}

func rowResult(values ...any) fakeRow { return fakeRow{values: values} }
func errRow(err error) fakeRow        { return fakeRow{err: err} }
func repeatRow(r fakeRow) []fakeRow   { return []fakeRow{r} }
func queueRow(r ...fakeRow) []fakeRow { return r }

func checkpointValues(timed, requested, checkpointBuffers, cleanBuffers int64, backendBuffers *int64,
	checkpointerReset, bgwriterReset time.Time,
) fakeRow {
	return rowResult(
		ptr(timed), ptr(requested), ptr(checkpointBuffers), ptr(cleanBuffers), backendBuffers,
		&checkpointerReset, &bgwriterReset,
	)
}

func ordersCheckpointsPre17() []fakeRow {
	return queueRow(
		checkpointValues(842, 12, 1204882, 88104, ptr(int64(310884)),
			testDBStatsReset, testDBStatsReset),
		checkpointValues(842, 15, 1205410, 88220, ptr(int64(311002)),
			testDBStatsReset, testDBStatsReset),
	)
}

func ordersCheckpointsPG17() []fakeRow {
	return queueRow(
		checkpointValues(842, 12, 1204882, 88104, nil, testDBStatsReset, testBgwriterReset),
		checkpointValues(842, 15, 1205410, 88220, nil, testDBStatsReset, testBgwriterReset),
	)
}

func connectionGroup(application, backendType string, connections, total int64) []any {
	return []any{ptr(application), ptr(backendType), connections, total}
}

func ordersConnections() [][]any {
	const total = 10

	return [][]any{
		connectionGroup("orders-service", "client backend", 86, total),
		connectionGroup("inventory-service", "client backend", 31, total),
		connectionGroup("reporting-worker", "client backend", 14, total),
		connectionGroup("", "client backend", 11, total),
		connectionGroup("", "autovacuum launcher", 1, total),
		connectionGroup("", "background writer", 1, total),
		connectionGroup("", "checkpointer", 1, total),
		connectionGroup("", "logical replication launcher", 1, total),
		connectionGroup("", "walwriter", 1, total),
		connectionGroup(ApplicationName, "client backend", 1, total),
	}
}

type fakeCapacityConn struct {
	*fakeWindowConn

	checkpoint      []fakeRow
	checkpointPre17 []fakeRow
	connections     []fakeResult
	wal             []fakeRow

	sql             []string
	connectionsArgs [][]any
}

func newFakeCapacityConn() *fakeCapacityConn {
	return &fakeCapacityConn{
		fakeWindowConn:  newFakeWindowConn(),
		checkpoint:      ordersCheckpointsPG17(),
		checkpointPre17: ordersCheckpointsPre17(),
		connections:     repeat(rowsResult(ordersConnections())),
		wal:             repeatRow(rowResult(ptr(int64(2254857830)))),
	}
}

func (c *fakeCapacityConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.sql = append(c.sql, sql)

	switch sql {
	case checkpointSQL:
		return answerRow(&c.checkpoint)

	case checkpointSQLPre17:
		return answerRow(&c.checkpointPre17)

	case walSQL:
		return answerRow(&c.wal)
	}

	return c.fakeWindowConn.QueryRow(ctx, sql, args...)
}

func (c *fakeCapacityConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = append(c.sql, sql)

	if sql == connectionsSQL {
		c.connectionsArgs = append(c.connectionsArgs, args)

		return answer(&c.connections)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func capacityGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 61),
		at(32, 5, 61),
		at(34, 5, 55),
		at(34, 5, 70),
	)
}

func runCapacityWindow(t *testing.T, clock *scriptedClock, collector Capacity,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{collector},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func takeCapacitySample(t *testing.T, conn *fakeCapacityConn, collector Capacity) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf, capacitySampleContext(2, 2)))

	return buf.String()
}

func capacitySampleContext(index, total int) SampleContext {
	return SampleContext{
		At: at(34, 5, 55), Index: index, Total: total,
		Database: "orders_db", DBID: "16401",
		HasPgStatCheckpointer: true,
	}
}

func capacityBlocks(t *testing.T, sample string) map[string]capacityBlock {
	t.Helper()

	blocks := make(map[string]capacityBlock)

	var current string

	for line := range strings.SplitSeq(strings.TrimSuffix(sample, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			current = sourceOf(t, line)
			require.NotContains(t, blocks, current, "one block per source in one sample")
			blocks[current] = capacityBlock{header: line}

			continue
		}

		require.NotEmpty(t, current, "a body line before any block header")

		block := blocks[current]
		block.body = append(block.body, line)
		blocks[current] = block
	}

	return blocks
}

type capacityBlock struct {
	header string
	body   []string
}

func (b capacityBlock) rows(t *testing.T, columns []string) [][]string {
	t.Helper()

	records, err := csv.NewReader(strings.NewReader(strings.Join(b.body, "\n"))).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records, "a block always writes its column header")
	require.Equal(t, columns, records[0])

	return records[1:]
}

func sourceOf(t *testing.T, header string) string {
	t.Helper()

	for _, token := range strings.Fields(header) {
		if name, ok := strings.CutPrefix(token, "source="); ok {
			return name
		}
	}

	t.Fatalf("block header has no source=: %s", header)

	return ""
}

func TestCapacityArtifact(t *testing.T) {
	artifact := Capacity{}.Artifact()

	assert.Equal(t, "pg_capacity", artifact.Name)
	assert.Equal(t, "pg_capacity.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"checkpoints, connections and WAL are the server's, not the connected database's")
	assert.Equal(t, StartEnd(), artifact.Schedule)

	assert.Equal(t, 3*StatementTimeout, artifact.SampleBudget,
		"three statements on the closing sample, which is what moduleDeadline sums - "+
			"leaving it zero would size the shared tick for two")
}

func TestCapacityColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"checkpoints_timed",
		"checkpoints_req",
		"buffers_checkpoint",
		"buffers_clean",
		"buffers_backend",
		"checkpointer_stats_reset",
		"bgwriter_stats_reset",
	}, checkpointColumns, "the pre-17 names on every version, and both reset clocks")

	assert.Equal(t, []string{"application_name", "backend_type", "active_connections"},
		connectionColumns, "backend_type is a grouping dimension, so it is in the contract")

	assert.Equal(t, []string{"wal_bytes"}, walColumns)
}

func TestCapacitySelectsTheStatementOnTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasView  bool
		want     string
		unwanted string
		views    string
	}{
		{
			name:    "pg_stat_checkpointer exists, so the counters are read from it",
			hasView: true, want: checkpointSQL, unwanted: checkpointSQLPre17,
			views: "views=pg_stat_checkpointer,pg_stat_bgwriter",
		},
		{
			name:    "it does not, so they are read from pg_stat_bgwriter",
			hasView: false, want: checkpointSQLPre17, unwanted: checkpointSQL,
			views: "views=pg_stat_bgwriter",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeCapacityConn()

			sampleCtx := capacitySampleContext(1, 2)
			sampleCtx.HasPgStatCheckpointer = tc.hasView

			var buf bytes.Buffer
			require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, sampleCtx))

			assert.Contains(t, conn.sql, tc.want, "the statement the server can answer")
			assert.NotContains(t, conn.sql, tc.unwanted, "and only that one")

			assert.Contains(t, buf.String(), tc.views,
				"views= is the provenance, and it is what a reader compares two servers with")
			assert.Contains(t, buf.String(), "source=pg_checkpointer",
				"source= is the parser's dispatch key and does not move with the version")
		})
	}
}

func TestCapacityWritesTheSameColumnsOnBothPaths(t *testing.T) {
	read := func(hasView bool) [][]string {
		t.Helper()

		conn := newFakeCapacityConn()

		sampleCtx := capacitySampleContext(1, 2)
		sampleCtx.HasPgStatCheckpointer = hasView

		var buf bytes.Buffer
		require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, sampleCtx))

		return capacityBlocks(t, buf.String())["pg_checkpointer"].rows(t, checkpointColumns)
	}

	pre17 := read(false)
	pg17 := read(true)

	require.Len(t, pre17, 1)
	require.Len(t, pg17, 1)

	assert.Equal(t, "310884", pre17[0][colBuffersBackend],
		"the column is a reading below 17")
	assert.Empty(t, pg17[0][colBuffersBackend],
		"and empty at and above it: 0 would mean backends wrote no buffers, which is a finding, "+
			"where the truth is that PostgreSQL 17 stopped counting")

	for _, column := range []int{colCheckpointsTimed, colCheckpointsReq, colBuffersCheckpoint, colBuffersClean} {
		assert.Equal(t, pre17[0][column], pg17[0][column],
			"every counter but buffers_backend is normalised to one name and one meaning")
	}
}

func TestCapacityWritesBothResetClocks(t *testing.T) {
	conn := newFakeCapacityConn()

	sampleCtx := capacitySampleContext(1, 2)

	var buf bytes.Buffer
	require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, sampleCtx))

	rows := capacityBlocks(t, buf.String())["pg_checkpointer"].rows(t, checkpointColumns)
	require.Len(t, rows, 1)

	assert.Equal(t, "2026-07-20T02:00:00.000Z", rows[0][colCheckpointerReset])
	assert.Equal(t, "2026-08-01T09:15:00.000Z", rows[0][colBgwriterReset],
		"on 17 and above the two views reset independently, so one column would leave the "+
			"other view's counter with an undetectable reset")

	sampleCtx.HasPgStatCheckpointer = false

	buf.Reset()
	require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, sampleCtx))

	rows = capacityBlocks(t, buf.String())["pg_checkpointer"].rows(t, checkpointColumns)
	require.Len(t, rows, 1)

	assert.Equal(t, rows[0][colCheckpointerReset], rows[0][colBgwriterReset],
		"below 17 they are one view's column read twice, and equal by construction")
	assert.NotEmpty(t, rows[0][colCheckpointerReset], "which is a value, not two empty cells")
}

func TestCapacityWritesTheGaugesOnlyAsTheWindowCloses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		index, total int
		want         []string
	}{
		{
			name:  "the opening sample writes the counters alone",
			index: 1, total: 2,
			want: []string{"pg_checkpointer"},
		},
		{
			name:  "the closing sample writes all three",
			index: 2, total: 2,
			want: []string{"pg_checkpointer", "pg_stat_activity_by_app", "pg_ls_waldir"},
		},
		{
			name:  "and a window with one sample writes all three at t0",
			index: 1, total: 1,
			want: []string{"pg_checkpointer", "pg_stat_activity_by_app", "pg_ls_waldir"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, Capacity{}.Sample(context.Background(), newFakeCapacityConn(), &buf,
				capacitySampleContext(tc.index, tc.total)))

			blocks := capacityBlocks(t, buf.String())

			sources := make([]string, 0, len(blocks))
			for source := range blocks {
				sources = append(sources, source)
			}

			assert.ElementsMatch(t, tc.want, sources)
		})
	}
}

func TestCapacityBlocksFailIndependently(t *testing.T) {
	denied := errors.New("ERROR: permission denied for function pg_ls_waldir (SQLSTATE 42501)")
	timedOut := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	t.Run("the WAL read alone", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.wal = repeatRow(errRow(denied))

		blocks := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))

		assert.Contains(t, blocks["pg_ls_waldir"].header,
			`error="ERROR: permission denied for function pg_ls_waldir (SQLSTATE 42501)"`,
			"driver text is quoted, so it cannot break k=v tokenisation")
		assert.Equal(t, []string{"wal_bytes"}, blocks["pg_ls_waldir"].body,
			"the column header with no row: captured nothing, and the header says why")

		assert.Len(t, blocks["pg_checkpointer"].rows(t, checkpointColumns), 1,
			"the counters read successfully beside it are unaffected")
		assert.Len(t, blocks["pg_stat_activity_by_app"].rows(t, connectionColumns), 10)
	})

	t.Run("the checkpoint read alone", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.checkpoint = repeatRow(errRow(timedOut))

		blocks := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))

		assert.Contains(t, blocks["pg_checkpointer"].header, "error=")
		assert.Contains(t, blocks["pg_checkpointer"].header, "views=pg_stat_checkpointer,pg_stat_bgwriter",
			"which variant was attempted is what explains the error beside it")
		assert.Empty(t, blocks["pg_checkpointer"].rows(t, checkpointColumns))

		assert.Len(t, blocks["pg_stat_activity_by_app"].rows(t, connectionColumns), 10,
			"the gauges still land: they have exactly one chance in the run")
		assert.Len(t, blocks["pg_ls_waldir"].rows(t, walColumns), 1)
	})

	t.Run("every read in the sample", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.checkpoint = repeatRow(errRow(timedOut))
		conn.connections = repeat(errResult(timedOut))
		conn.wal = repeatRow(errRow(denied))

		var buf bytes.Buffer
		require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, capacitySampleContext(2, 2)),
			"three refused reads are not an error: Sample fails only when it cannot write")

		blocks := capacityBlocks(t, buf.String())
		require.Len(t, blocks, 3,
			"three header-only blocks carrying three reasons, rather than one stub saying "+
				"the sample could not be taken")

		for source, columns := range map[string][]string{
			"pg_checkpointer":         checkpointColumns,
			"pg_stat_activity_by_app": connectionColumns,
			"pg_ls_waldir":            walColumns,
		} {
			assert.Contains(t, blocks[source].header, "error=", source)
			assert.Empty(t, blocks[source].rows(t, columns), source)
		}

		assert.NotContains(t, blocks["pg_stat_activity_by_app"].header, "groups_total",
			"groups_total=0 would assert the server has no connections, where the truth is "+
				"that nobody could count them")
	})
}

func TestCapacityFailedSampleIsStillACompleteSample(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.checkpoint = repeatRow(errRow(errors.New("ERROR: permission denied")))
	conn.connections = repeat(errResult(errors.New("ERROR: permission denied")))
	conn.wal = repeatRow(errRow(errors.New("ERROR: permission denied")))

	results := runCapacityWindow(t, capacityGoldenClock(t), Capacity{}, connectTo(conn))

	assert.Equal(t, StatusComplete, results[0].Status,
		"a sample of degraded blocks is a sample: the window's stub is for a collector that "+
			"cannot localise a failure, and this one always can")
	assert.Equal(t, 2, results[0].SamplesWritten)
	assert.NotContains(t, artifactText(t, results[0]), "sample_error=", "so no stub was written")
}

func TestCapacitySampleErrorsOnlyOnAWriteFailure(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	err := Capacity{}.Sample(context.Background(), newFakeCapacityConn(), failingWriter{err: sinkErr},
		capacitySampleContext(2, 2))

	require.ErrorIs(t, err, sinkErr, "which the window turns into IOErr rather than into a stub")
}

func TestCapacityWritesTheWholeSampleInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Capacity{}.Sample(context.Background(), newFakeCapacityConn(), writer,
		capacitySampleContext(2, 2)))

	assert.Equal(t, 1, writer.writes,
		"three blocks, one buffer, one Write: a write failing between two of them would leave "+
			"the window's stub behind a half-written sample")
	assert.Equal(t, 3, strings.Count(writer.buf.String(), "# engine=postgres"))
}

func TestCapacityIssuesTheStatementsItsBudgetIsDeclaredFor(t *testing.T) {
	opening := newFakeCapacityConn()
	require.NoError(t, Capacity{}.Sample(context.Background(), opening, io.Discard,
		capacitySampleContext(1, 2)))

	assert.Equal(t, []string{checkpointSQL}, opening.sql)

	closing := newFakeCapacityConn()
	require.NoError(t, Capacity{}.Sample(context.Background(), closing, io.Discard,
		capacitySampleContext(2, 2)))

	assert.Equal(t, []string{checkpointSQL, connectionsSQL, walSQL}, closing.sql,
		"three statements, which is what Artifact().SampleBudget declares and what "+
			"Window.moduleDeadline sizes the shared closing tick from")
}

func TestCapacityConnectionBlockGroupsRatherThanFilters(t *testing.T) {
	assert.NotContains(t, connectionsSQL, "WHERE",
		"pg_stat_activity is read unfiltered: a WHERE backend_type = 'client backend' would give "+
			"the report its headline number and destroy the evidence underneath it")

	block := capacityBlocks(t, takeCapacitySample(t, newFakeCapacityConn(), Capacity{}))["pg_stat_activity_by_app"]

	assert.Contains(t, block.header, "groups_written=10 groups_total=10 truncated=false")

	rows := block.rows(t, connectionColumns)
	require.Len(t, rows, 10)

	assert.Equal(t, []string{"orders-service", "client backend", "86"}, rows[0])
	assert.Equal(t, []string{"", "client backend", "11"}, rows[3],
		"a connection that never set an application_name is an empty string, not a NULL")
	assert.Equal(t, []string{"", "checkpointer", "1"}, rows[6],
		"a background process is its own row: under an unfiltered GROUP BY application_name it "+
			"was an unnamed client connection, in a number read against max_connections")
	assert.Equal(t, []string{ApplicationName, "client backend", "1"}, rows[9],
		"the agent's own connection stays, so the block agrees with a hand-run count(*)")
}

func TestCapacityMaskedBackendTypeRendersEmpty(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.connections = repeat(rowsResult([][]any{
		{ptr(""), nil, int64(5), int64(2)},
		connectionGroup(ApplicationName, "client backend", 1, 2),
	}))

	rows := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_stat_activity_by_app"].
		rows(t, connectionColumns)
	require.Len(t, rows, 2)

	assert.Equal(t, []string{"", "", "5"}, rows[0],
		"a role without pg_read_all_stats sees every row but has backend_type masked to NULL and "+
			"application_name left empty - the count is still right and the grain collapsed")
	assert.Equal(t, []string{ApplicationName, "client backend", "1"}, rows[1],
		"and it sees its own backend in full")
}

func TestCapacityCapKeepsTheGroupsHoldingTheConnections(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.connections = repeat(rowsResult([][]any{
		connectionGroup("orders-service", "client backend", 86, 4120),
		connectionGroup("inventory-service", "client backend", 31, 4120),
		connectionGroup("reporting-worker", "client backend", 14, 4120),
	}))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{MaxConnectionGroups: 3}))["pg_stat_activity_by_app"]

	assert.Contains(t, block.header, "groups_written=3 groups_total=4120 truncated=true",
		"a capped block must not read as a complete one")

	rows := block.rows(t, connectionColumns)
	require.Len(t, rows, 3)

	assert.Equal(t, []string{"86", "31", "14"}, []string{
		rows[0][colActiveConnections], rows[1][colActiveConnections], rows[2][colActiveConnections],
	}, "the survivors are the groups holding the connections, which is what the statement's "+
		"ORDER BY count(*) DESC is for")

	require.Len(t, conn.connectionsArgs, 1)
	assert.Equal(t, []any{3}, conn.connectionsArgs[0], "the cap is the LIMIT the server is sent")
	assert.Contains(t, connectionsSQL, "ORDER BY count(*) DESC, application_name, backend_type",
		"asserted on the statement, because the fake cannot rank for the server")
}

func TestCapacityDefaultCapIsSentWhenUnset(t *testing.T) {
	conn := newFakeCapacityConn()

	takeCapacitySample(t, conn, Capacity{})

	require.Len(t, conn.connectionsArgs, 1)
	assert.Equal(t, DefaultMaxConnectionGroups, conn.connectionsArgs[0][0])
}

func TestCapacityApplicationNamesWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.connections = repeat(rowsResult([][]any{
		connectionGroup("we,ird\"app\nname", "client backend", 3, 1),
	}))

	rows := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_stat_activity_by_app"].
		rows(t, connectionColumns)
	require.Len(t, rows, 1)

	assert.Equal(t, "we,ird\"app name", rows[0][colApplicationName],
		"application_name is client-chosen and arrives arbitrary")
}

func TestCapacityWALSumOfNothingWritesNoRowRatherThanZero(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.wal = repeatRow(rowResult(nil))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_ls_waldir"]

	assert.Empty(t, block.rows(t, walColumns),
		"sum() is NULL over a directory with no files, and 0 would be a reading. This is the "+
			"only single-column body in the package, so one empty cell would be a blank line "+
			"that a CSV reader skips - the column header alone says captured-and-found-nothing")
	assert.NotContains(t, block.header, "error=",
		"and the absence of error= is what separates that from could-not-be-captured")
}

func TestCapacityGoldenPG17(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = true

	results := runCapacityWindow(t, capacityGoldenClock(t), Capacity{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 2, results[0].SamplesWritten, "two samples, four sample blocks")
	assert.Equal(t, bloatGolden(t, "pg_capacity_pg17.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenPre17(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = false

	results := runCapacityWindow(t, capacityGoldenClock(t), Capacity{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_capacity_pre17.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenWALDenied(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = true
	conn.wal = repeatRow(errRow(errors.New(
		"ERROR: permission denied for function pg_ls_waldir (SQLSTATE 42501)")))

	results := runCapacityWindow(t, capacityGoldenClock(t), Capacity{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"the least-privilege role gets two populated blocks and one that says why it is empty")
	assert.Equal(t, bloatGolden(t, "pg_capacity_wal_denied.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runCapacityWindow(t, clock, Capacity{},
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_capacity_connect_failure.txt"), artifactText(t, results[0]))
}
