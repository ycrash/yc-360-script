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
	colApplicationName = iota
	colBackendType
	colActiveConnections
)

var (
	testBgwriterReset = time.Date(2026, 8, 1, 9, 15, 0, 0, time.UTC)
	testIOReset       = time.Date(2026, 8, 3, 11, 40, 0, 0, time.UTC)
)

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

// testCheckpointsTimed holds still across the window: only requested checkpoints move.
const testCheckpointsTimed int64 = 842

// ordersCheckpoints is one window's two samples, read below 17 from one view and from 17 from three.
var ordersCheckpoints = []struct{ requested, written, clean, backend int64 }{
	{requested: 12, written: 1204882, clean: 88104, backend: 310884},
	{requested: 15, written: 1205410, clean: 88220, backend: 311002},
}

func ordersBgwriterPre17() []fakeRow {
	rows := make([]fakeRow, 0, len(ordersCheckpoints))
	for _, c := range ordersCheckpoints {
		rows = append(rows, rowResult(ptr(testCheckpointsTimed), ptr(c.requested), ptr(c.written), ptr(c.clean),
			ptr(c.backend), &testDBStatsReset))
	}

	return queueRow(rows...)
}

func ordersCheckpointer() []fakeRow {
	rows := make([]fakeRow, 0, len(ordersCheckpoints))
	for _, c := range ordersCheckpoints {
		rows = append(rows, rowResult(ptr(testCheckpointsTimed), ptr(c.requested), ptr(c.written), &testDBStatsReset))
	}

	return queueRow(rows...)
}

func ordersBgwriter() []fakeRow {
	rows := make([]fakeRow, 0, len(ordersCheckpoints))
	for _, c := range ordersCheckpoints {
		rows = append(rows, rowResult(ptr(c.clean), &testBgwriterReset))
	}

	return queueRow(rows...)
}

func ordersBackendBuffers() []fakeRow {
	rows := make([]fakeRow, 0, len(ordersCheckpoints))
	for _, c := range ordersCheckpoints {
		rows = append(rows, rowResult(ptr(c.backend), &testIOReset))
	}

	return queueRow(rows...)
}

// ordersDatabase is orders_db's row in pg_health's samples, so the two files agree.
func ordersDatabase() []fakeRow {
	return queueRow(
		rowResult(ptr(int64(442198)), ptr(int64(9532)), ptr(int64(8823401)), ptr(int64(158220)),
			ptr(int64(268435456))),
		rowResult(ptr(int64(442340)), ptr(int64(9538)), ptr(int64(8841820)), ptr(int64(158910)),
			ptr(int64(356515840))),
	)
}

func connectionGroup(application, backendType string, connections, total int64) []any {
	return []any{ptr(application), ptr(backendType), connections, total}
}

// ordersConnections is in the statement's own order - application_name, then
// backend_type - so the goldens show what a server returns: the unnamed groups first.
func ordersConnections() [][]any {
	const total = 10

	return [][]any{
		connectionGroup("", "autovacuum launcher", 1, total),
		connectionGroup("", "background writer", 1, total),
		connectionGroup("", "checkpointer", 1, total),
		connectionGroup("", "client backend", 11, total),
		connectionGroup("", "logical replication launcher", 1, total),
		connectionGroup("", "walwriter", 1, total),
		connectionGroup("inventory-service", "client backend", 31, total),
		connectionGroup("orders-service", "client backend", 86, total),
		connectionGroup("reporting-worker", "client backend", 14, total),
		connectionGroup(ApplicationName, "client backend", 1, total),
	}
}

type fakeCapacityConn struct {
	*fakeWindowConn

	bgwriterPre17  []fakeRow
	checkpointer   []fakeRow
	bgwriter       []fakeRow
	backendBuffers []fakeRow
	database       []fakeRow
	connections    []fakeResult
	walAllowed     []fakeRow
	wal            []fakeRow

	sql             []string
	connectionsArgs [][]any
}

func newFakeCapacityConn() *fakeCapacityConn {
	return &fakeCapacityConn{
		fakeWindowConn: newFakeWindowConn(),
		bgwriterPre17:  ordersBgwriterPre17(),
		checkpointer:   ordersCheckpointer(),
		bgwriter:       ordersBgwriter(),
		backendBuffers: ordersBackendBuffers(),
		database:       ordersDatabase(),
		connections:    repeat(rowsResult(ordersConnections())),
		walAllowed:     repeatRow(rowResult(ptr(true))),
		wal:            repeatRow(rowResult(ptr(int64(2254857830)))),
	}
}

func (c *fakeCapacityConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.sql = append(c.sql, sql)

	switch sql {
	case bgwriterSQLPre17:
		return answerRow(&c.bgwriterPre17)

	case checkpointerSQL:
		return answerRow(&c.checkpointer)

	case bgwriterSQL:
		return answerRow(&c.bgwriter)

	case backendBuffersSQL:
		return answerRow(&c.backendBuffers)

	case databaseSQL:
		return answerRow(&c.database)

	case walPrivilegeSQL:
		return answerRow(&c.walAllowed)

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

func runCapacityWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:         testTarget(),
		Duration:       120 * time.Second,
		Collectors:     []Collector{Capacity{}},
		now:            clock.now,
		CaptureID:      testCaptureID,
		statementClock: steppedStatements,
		after:          clock.after,
		connect:        connect,
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

	var current, capture string

	for line := range strings.SplitSeq(strings.TrimSuffix(sample, "\n"), "\n") {
		if strings.HasPrefix(line, "# capture_id=") {
			require.Empty(t, capture, "a capture line is followed by its sample line")
			capture = line

			continue
		}

		if strings.HasPrefix(line, "#") {
			require.NotEmpty(t, capture, "a sample line follows its capture line")

			current = sourceOf(t, line)
			require.NotContains(t, blocks, current, "one block per source in one sample")
			blocks[current] = capacityBlock{header: capture + "\n" + line}
			capture = ""

			continue
		}

		require.NotEmpty(t, current, "a body line before any block header")

		block := blocks[current]
		block.body = append(block.body, line)
		blocks[current] = block
	}

	return blocks
}

// capacityBlock's header is both of its header lines.
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
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), Capacity{Interval: 15 * time.Second}.Artifact().Schedule,
		"the run's cadence, with the close as the last sample")

	assert.Equal(t, 7*StatementTimeout, artifact.SampleBudget,
		"seven statements on every sample from 17, and Periodic's last sample is the close, "+
			"which moduleDeadline sums - leaving it zero would size the shared tick for two")

	assert.Equal(t, 3, artifact.Version,
		"the checkpoint counters' blocks and columns were renamed at 2, and a sample block's "+
			"header became two lines at 3; a reader of either earlier version gets this one wrong")
}

func TestCapacityColumnOrder(t *testing.T) {
	require.Len(t, checkpointBlocksPre17, 1)
	assert.Equal(t, "pg_stat_bgwriter", checkpointBlocksPre17[0].source)
	assert.Equal(t, []string{
		"checkpoints_timed", "checkpoints_req", "buffers_checkpoint", "buffers_clean", "buffers_backend",
		"stats_reset",
	}, checkpointBlocksPre17[0].columns, "below 17 one view held every counter")

	sources := make([]string, 0, len(checkpointBlocks17))
	for _, block := range checkpointBlocks17 {
		sources = append(sources, block.source)
	}

	assert.Equal(t, []string{"pg_stat_checkpointer", "pg_stat_bgwriter", "pg_stat_io"}, sources,
		"from 17 three views hold them, each block named by its view")
	assert.Equal(t, []string{"num_timed", "num_requested", "buffers_written", "stats_reset"},
		checkpointBlocks17[0].columns, "each view's own column names")
	assert.Equal(t, []string{"buffers_clean", "stats_reset"}, checkpointBlocks17[1].columns)
	assert.Equal(t, []string{"buffers_backend", "stats_reset"}, checkpointBlocks17[2].columns)

	assert.Equal(t, []string{"application_name", "backend_type", "active_connections"},
		connectionColumns, "backend_type is a grouping dimension, so it is in the contract")

	assert.Equal(t, []string{"wal_bytes"}, walColumns)

	assert.Equal(t, []string{"xact_commit", "xact_rollback", "blks_hit", "blks_read", "temp_bytes"},
		databaseColumns, "the counters of the cache hit, rollback and throughput ratios")
}

func TestCapacitySelectsTheStatementsOnTheCapability(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hasView  bool
		want     []string
		unwanted []string
		sources  []string
	}{
		{
			name:    "pg_stat_checkpointer exists, so the counters are read from the three views",
			hasView: true, want: []string{checkpointerSQL, bgwriterSQL, backendBuffersSQL},
			unwanted: []string{bgwriterSQLPre17},
			sources:  []string{"pg_stat_checkpointer", "pg_stat_bgwriter", "pg_stat_io"},
		},
		{
			name:    "it does not, so they are read from pg_stat_bgwriter alone",
			hasView: false, want: []string{bgwriterSQLPre17},
			unwanted: []string{checkpointerSQL, bgwriterSQL, backendBuffersSQL},
			sources:  []string{"pg_stat_bgwriter"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeCapacityConn()

			sampleCtx := capacitySampleContext(1, 2)
			sampleCtx.HasPgStatCheckpointer = tc.hasView

			var buf bytes.Buffer
			require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, sampleCtx))

			for _, sql := range tc.want {
				assert.Contains(t, conn.sql, sql, "the statements the server can answer")
			}

			for _, sql := range tc.unwanted {
				assert.NotContains(t, conn.sql, sql, "and only those")
			}

			blocks := capacityBlocks(t, buf.String())
			for _, source := range tc.sources {
				assert.Contains(t, blocks, source, "source= names the view the block read")
			}

			assert.Len(t, blocks, len(tc.sources)+3, "beside the database, connection and WAL blocks")
			assert.NotContains(t, buf.String(), "views=", "source= says which view, so views= went")
		})
	}
}

func TestCapacityCheckpointBlocksKeepEachViewsNames(t *testing.T) {
	read := func(hasView bool) map[string][]string {
		t.Helper()

		sampleCtx := capacitySampleContext(1, 2)
		sampleCtx.HasPgStatCheckpointer = hasView

		var buf bytes.Buffer
		require.NoError(t, Capacity{}.Sample(context.Background(), newFakeCapacityConn(), &buf, sampleCtx))

		values := map[string][]string{}

		for source, block := range capacityBlocks(t, buf.String()) {
			for _, candidate := range checkpointBlocks(hasView) {
				if candidate.source != source {
					continue
				}

				rows := block.rows(t, candidate.columns)
				require.Len(t, rows, 1, source)

				for i, column := range candidate.columns {
					values[source+"."+column] = append(values[source+"."+column], rows[0][i])
				}
			}
		}

		return values
	}

	pre17 := read(false)
	pg17 := read(true)

	for pre17Column, pg17Column := range map[string]string{
		"pg_stat_bgwriter.checkpoints_timed":  "pg_stat_checkpointer.num_timed",
		"pg_stat_bgwriter.checkpoints_req":    "pg_stat_checkpointer.num_requested",
		"pg_stat_bgwriter.buffers_checkpoint": "pg_stat_checkpointer.buffers_written",
		"pg_stat_bgwriter.buffers_clean":      "pg_stat_bgwriter.buffers_clean",
		"pg_stat_bgwriter.buffers_backend":    "pg_stat_io.buffers_backend",
	} {
		require.NotEmpty(t, pre17[pre17Column], pre17Column)
		assert.Equal(t, pre17[pre17Column], pg17[pg17Column],
			"the same counter under the name its view gives it: mapping one to the other is the server's")
	}
}

func TestCapacityCountsBackendBuffersFromPgStatIOOnPG17(t *testing.T) {
	assert.Contains(t, backendBuffersSQL, "COALESCE(writes, 0) + COALESCE(extends, 0)",
		"writes and extensions, as the old column counted, and a NULL extends on a bulkread "+
			"row must not drop that row's writes")
	assert.Contains(t, backendBuffersSQL, "object = 'relation'",
		"temporary relations were never counted: they are not synced")
	assert.Contains(t, backendBuffersSQL, "NOT IN ('checkpointer', 'background writer')",
		"every other process's writes counted, parallel and autovacuum workers included")

	conn := newFakeCapacityConn()
	conn.backendBuffers = repeatRow(rowResult(nil, &testIOReset))

	var buf bytes.Buffer
	require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, capacitySampleContext(1, 2)))

	rows := capacityBlocks(t, buf.String())["pg_stat_io"].rows(t, checkpointBlocks17[2].columns)
	require.Len(t, rows, 1)
	assert.Equal(t, []string{"", "2026-08-03T11:40:00.000Z"}, rows[0],
		"no pg_stat_io row to sum is empty, not 0: 0 would say backends wrote no buffers")
}

func TestCapacityEachCheckpointBlockCarriesItsViewsResetClock(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, Capacity{}.Sample(context.Background(), newFakeCapacityConn(), &buf,
		capacitySampleContext(1, 2)))

	blocks := capacityBlocks(t, buf.String())

	for i, want := range []string{"2026-07-20T02:00:00.000Z", "2026-08-01T09:15:00.000Z", "2026-08-03T11:40:00.000Z"} {
		block := checkpointBlocks17[i]

		rows := blocks[block.source].rows(t, block.columns)
		require.Len(t, rows, 1)
		assert.Equal(t, want, rows[0][len(block.columns)-1],
			"%s: on 17 and above the three views reset independently, so one clock would leave "+
				"the others' counters with an undetectable reset", block.source)
	}
}

func TestCapacityWritesEveryBlockOnEverySample(t *testing.T) {
	for _, tc := range []struct {
		name         string
		index, total int
	}{
		{name: "the opening sample", index: 1, total: 3},
		{name: "a sample between the endpoints", index: 2, total: 3},
		{name: "the closing sample", index: 3, total: 3},
		{name: "and a window with one sample", index: 1, total: 1},
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

			assert.ElementsMatch(t,
				[]string{
					"pg_stat_checkpointer", "pg_stat_bgwriter", "pg_stat_io",
					"pg_stat_database", "pg_stat_activity_by_app", "pg_ls_waldir",
				}, sources,
				"the gauges once landed on the closing sample alone; as a series they show "+
					"connections climbing and WAL growing through the window")
		})
	}
}

func TestCapacityBlocksFailIndependently(t *testing.T) {
	denied := errors.New("ERROR: permission denied for function pg_ls_waldir (SQLSTATE 42501)")
	timedOut := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	t.Run("the WAL read alone, refused after the privilege check passed", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.wal = repeatRow(errRow(denied))

		blocks := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))

		assert.Contains(t, blocks["pg_ls_waldir"].header,
			`error="ERROR: permission denied for function pg_ls_waldir (SQLSTATE 42501)"`,
			"driver text is quoted, so it cannot break k=v tokenisation")
		assert.Equal(t, []string{"wal_bytes"}, blocks["pg_ls_waldir"].body,
			"the column header with no row: captured nothing, and the header says why")

		assert.Len(t, blocks["pg_stat_checkpointer"].rows(t, checkpointBlocks17[0].columns), 1,
			"the counters read successfully beside it are unaffected")
		assert.Len(t, blocks["pg_stat_activity_by_app"].rows(t, connectionColumns), 10)
	})

	t.Run("one checkpoint view alone", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.backendBuffers = repeatRow(errRow(timedOut))

		blocks := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))

		assert.Contains(t, blocks["pg_stat_io"].header, "error=")
		assert.Empty(t, blocks["pg_stat_io"].rows(t, checkpointBlocks17[2].columns))

		assert.Len(t, blocks["pg_stat_checkpointer"].rows(t, checkpointBlocks17[0].columns), 1,
			"each view is its own read, so the other two still land")
		assert.Len(t, blocks["pg_stat_bgwriter"].rows(t, checkpointBlocks17[1].columns), 1)
		assert.Len(t, blocks["pg_stat_activity_by_app"].rows(t, connectionColumns), 10)
		assert.Len(t, blocks["pg_ls_waldir"].rows(t, walColumns), 1)
	})

	t.Run("every read in the sample", func(t *testing.T) {
		conn := newFakeCapacityConn()
		conn.checkpointer = repeatRow(errRow(timedOut))
		conn.bgwriter = repeatRow(errRow(timedOut))
		conn.backendBuffers = repeatRow(errRow(timedOut))
		conn.database = repeatRow(errRow(timedOut))
		conn.connections = repeat(errResult(timedOut))
		conn.wal = repeatRow(errRow(denied))

		var buf bytes.Buffer
		require.NoError(t, Capacity{}.Sample(context.Background(), conn, &buf, capacitySampleContext(2, 2)),
			"six refused reads are not an error: Sample fails only when it cannot write")

		blocks := capacityBlocks(t, buf.String())
		require.Len(t, blocks, 6,
			"six header-only blocks carrying six reasons, rather than one stub saying "+
				"the sample could not be taken")

		for source, columns := range map[string][]string{
			"pg_stat_checkpointer":    checkpointBlocks17[0].columns,
			"pg_stat_bgwriter":        checkpointBlocks17[1].columns,
			"pg_stat_io":              checkpointBlocks17[2].columns,
			"pg_stat_database":        databaseColumns,
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
	conn.checkpointer = repeatRow(errRow(errors.New("ERROR: permission denied")))
	conn.bgwriter = repeatRow(errRow(errors.New("ERROR: permission denied")))
	conn.backendBuffers = repeatRow(errRow(errors.New("ERROR: permission denied")))
	conn.database = repeatRow(errRow(errors.New("ERROR: permission denied")))
	conn.connections = repeat(errResult(errors.New("ERROR: permission denied")))
	conn.wal = repeatRow(errRow(errors.New("ERROR: permission denied")))

	results := runCapacityWindow(t, capacityGoldenClock(t), connectTo(conn))

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
		"six blocks, one buffer, one Write: a write failing between two of them would leave "+
			"the window's stub behind a half-written sample")
	assert.Equal(t, 6, strings.Count(writer.buf.String(), "# capture_id="))
}

func TestCapacityIssuesTheStatementsItsBudgetIsDeclaredFor(t *testing.T) {
	for _, s := range []SampleContext{capacitySampleContext(1, 2), capacitySampleContext(2, 2)} {
		conn := newFakeCapacityConn()
		require.NoError(t, Capacity{}.Sample(context.Background(), conn, io.Discard, s))

		assert.Equal(t, []string{
			checkpointerSQL, bgwriterSQL, backendBuffersSQL,
			databaseSQL, connectionsSQL, walPrivilegeSQL, walSQL,
		}, conn.sql,
			"sample %d: seven statements, which is what Artifact().SampleBudget declares and "+
				"what Window.moduleDeadline sizes the shared closing tick from", s.Index)
	}

	conn := newFakeCapacityConn()
	sampleCtx := capacitySampleContext(1, 2)
	sampleCtx.HasPgStatCheckpointer = false

	require.NoError(t, Capacity{}.Sample(context.Background(), conn, io.Discard, sampleCtx))
	assert.Equal(t, []string{bgwriterSQLPre17, databaseSQL, connectionsSQL, walPrivilegeSQL, walSQL}, conn.sql,
		"five below 17, inside the budget")
}

func TestCapacityStampsItsOwnVersionOnEveryBlock(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = true

	results := runCapacityWindow(t, capacityGoldenClock(t), connectTo(conn))

	blocks := 0

	for line := range strings.SplitSeq(artifactText(t, results[0]), "\n") {
		if strings.HasPrefix(line, "# engine=") || strings.HasPrefix(line, "# capture_id=") {
			blocks++

			assert.Contains(t, line, " v=3 ", "the window's blocks and the collector's alike")
		}
	}

	assert.Equal(t, 14, blocks, "the preamble, six blocks in each of two samples, and the close")
}

func TestCapacityDatabaseBlockIsTheConnectedDatabasesRow(t *testing.T) {
	assert.Contains(t, databaseSQL, "WHERE datname = current_database()",
		"the connected database alone: pg_health.txt has every database's row")

	block := capacityBlocks(t, takeCapacitySample(t, newFakeCapacityConn(), Capacity{}))["pg_stat_database"]

	assert.Contains(t, block.header, " db=orders_db dbid=16401\n# sample_id=2 source=pg_stat_database ")
	assert.Contains(t, block.header, " scope=database ",
		"the row is one database's, in a file whose other blocks are the server's")
	assert.Equal(t, [][]string{{"442198", "9532", "8823401", "158220", "268435456"}},
		block.rows(t, databaseColumns))
}

func TestCapacityDatabaseBlockFailsAlone(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.database = repeatRow(errRow(errors.New(
		"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")))

	blocks := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))

	assert.Contains(t, blocks["pg_stat_database"].header,
		`error="ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"`)
	assert.Empty(t, blocks["pg_stat_database"].rows(t, databaseColumns), "no row, not a row of zeroes")

	assert.Len(t, blocks["pg_stat_checkpointer"].rows(t, checkpointBlocks17[0].columns), 1,
		"the blocks beside it still land")
	assert.Len(t, blocks["pg_stat_activity_by_app"].rows(t, connectionColumns), 10)
	assert.Len(t, blocks["pg_ls_waldir"].rows(t, walColumns), 1)
}

func TestCapacityConnectionBlockGroupsRatherThanFilters(t *testing.T) {
	assert.NotContains(t, connectionsSQL, "WHERE",
		"pg_stat_activity is read unfiltered: a WHERE backend_type = 'client backend' would give "+
			"the report its headline number and destroy the evidence underneath it")

	block := capacityBlocks(t, takeCapacitySample(t, newFakeCapacityConn(), Capacity{}))["pg_stat_activity_by_app"]

	assert.Contains(t, block.header, "rows=10 truncated=false scope=cluster groups_written=10 groups_total=10")

	rows := block.rows(t, connectionColumns)
	require.Len(t, rows, 10)

	assert.Equal(t, []string{"", "autovacuum launcher", "1"}, rows[0],
		"identity order: the unnamed groups sort before any application")
	assert.Equal(t, []string{"", "client backend", "11"}, rows[3],
		"a connection that never set an application_name is an empty string, not a NULL")
	assert.Equal(t, []string{"", "checkpointer", "1"}, rows[2],
		"a background process is its own row: under an unfiltered GROUP BY application_name it "+
			"was an unnamed client connection, in a number read against max_connections")
	assert.Equal(t, []string{"orders-service", "client backend", "86"}, rows[7],
		"the group holding the most connections is wherever its name sorts")
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

func TestCapacityCapCutsOnIdentitySoSamplesAgree(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.connections = repeat(rowsResult([][]any{
		connectionGroup("", "checkpointer", 1, 4120),
		connectionGroup("inventory-service", "client backend", 31, 4120),
		connectionGroup("orders-service", "client backend", 86, 4120),
	}))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{MaxConnectionGroups: 3}))["pg_stat_activity_by_app"]

	assert.Contains(t, block.header, "rows=3 truncated=true scope=cluster groups_written=3 groups_total=4120",
		"a capped block must not read as a complete one")

	require.Len(t, block.rows(t, connectionColumns), 3)

	require.Len(t, conn.connectionsArgs, 1)
	assert.Equal(t, []any{3}, conn.connectionsArgs[0], "the cap is the LIMIT the server is sent")

	assert.Contains(t, connectionsSQL, "ORDER BY application_name, backend_type",
		"asserted on the statement, because the fake cannot sort for the server: the cap cuts "+
			"on identity, so every sample keeps the same groups and the server can read one "+
			"sample against the next")
	assert.NotContains(t, connectionsSQL, "count(*) DESC",
		"a cap ordered by count(*) was safe while the block was written once; sampled "+
			"repeatedly it would let two samples keep two different sets with nothing in "+
			"common to delta. What identity order costs: the groups holding the most "+
			"connections can fall past the cap, which groups_total= and truncated= report")
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

func TestCapacityWALReadSkippedWhenTheRoleMayNotRunIt(t *testing.T) {
	assert.Equal(t, `SELECT has_function_privilege('pg_catalog.pg_ls_waldir()', 'EXECUTE')`, walPrivilegeSQL)

	conn := newFakeCapacityConn()
	conn.walAllowed = repeatRow(rowResult(ptr(false)))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_ls_waldir"]

	assert.NotContains(t, conn.sql, walSQL,
		"a refused call is an ERROR and its STATEMENT in the server's own log, every sample")
	assert.Contains(t, block.header, "reason=permission_denied")
	assert.NotContains(t, block.header, "error=", "nothing failed: the read was not made")
	assert.Equal(t, []string{"wal_bytes"}, block.body, "the column header alone")
}

func TestCapacityWALReadIsTriedWhenTheCheckAnswersNULL(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.walAllowed = repeatRow(rowResult(nil))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_ls_waldir"]

	assert.Contains(t, conn.sql, walSQL, "only a false skips the read")
	assert.Len(t, block.rows(t, walColumns), 1)
}

func TestCapacityFailedPrivilegeCheckIsTheBlocksError(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.walAllowed = repeatRow(errRow(errors.New(
		"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")))

	block := capacityBlocks(t, takeCapacitySample(t, conn, Capacity{}))["pg_ls_waldir"]

	assert.NotContains(t, conn.sql, walSQL, "nothing is known, so nothing is tried")
	assert.Contains(t, block.header, `error="ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"`)
	assert.NotContains(t, block.header, "reason=", "a failed check is not a refusal")
	assert.Empty(t, block.rows(t, walColumns))
}

func TestCapacityGoldenPG17(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = true

	results := runCapacityWindow(t, capacityGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 2, results[0].SamplesWritten, "two samples, twelve sample blocks")
	assert.Equal(t, bloatGolden(t, "pg_capacity_pg17.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenPre17(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = false
	conn.engineVersion = "160008"

	results := runCapacityWindow(t, capacityGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_capacity_pre17.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenWALDenied(t *testing.T) {
	conn := newFakeCapacityConn()
	conn.hasPgStatCheckpointer = true
	conn.walAllowed = repeatRow(rowResult(ptr(false)))

	results := runCapacityWindow(t, capacityGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"on every sample, the least-privilege role gets two populated blocks and one that "+
			"says why it is empty")
	assert.Equal(t, bloatGolden(t, "pg_capacity_wal_denied.txt"), artifactText(t, results[0]))
}

func TestCapacityGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runCapacityWindow(t, clock,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_capacity_connect_failure.txt"), artifactText(t, results[0]))
}
