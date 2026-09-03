package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colIndexrelid = iota
	colIndexRelid
	colIndexRelName
	colIndexScan
	colIndexTupRead
	colIndexTupFetch
	colIndexSizeBytes
)

func indexRow(indexrelid, relid uint32, name string, scan, read, fetch *int64, total int64) []any {
	return []any{indexrelid, relid, name, scan, read, fetch, total}
}

func indexSizeRow(indexrelid uint32, size *int64) []any {
	return []any{indexrelid, size}
}

func counted(v int64) *int64 { return ptr(v) }

// The four indexes on the two tables pg_bloat_full.txt carries, kept consistent
// with it: relid 16390 is orders and 16482 is orders_line_items; each table's
// two idx_scan counts sum to the table's in that fixture, on both samples, and
// each table's two sizes sum to its index_size_bytes there. orders_status_idx is
// the finding the artifact exists for - an index nothing scans.
func ordersIndexesStart() [][]any {
	return [][]any{
		indexRow(16396, 16390, "orders_pkey", counted(4021884), counted(4102330), counted(4021884), 4),
		indexRow(16397, 16390, "orders_status_idx", counted(0), counted(0), counted(0), 4),
		indexRow(16487, 16482, "orders_line_items_pkey", counted(88104), counted(88104), counted(88104), 4),
		indexRow(16488, 16482, "orders_line_items_order_id_idx", counted(1112336), counted(2401103), counted(2400880), 4),
	}
}

func ordersIndexesEnd() [][]any {
	return [][]any{
		indexRow(16396, 16390, "orders_pkey", counted(4025901), counted(4106402), counted(4025901), 4),
		indexRow(16397, 16390, "orders_status_idx", counted(0), counted(0), counted(0), 4),
		indexRow(16487, 16482, "orders_line_items_pkey", counted(88110), counted(88110), counted(88110), 4),
		indexRow(16488, 16482, "orders_line_items_order_id_idx", counted(1113870), counted(2404210), counted(2403987), 4),
	}
}

func ordersIndexSizes() [][]any {
	return [][]any{
		indexSizeRow(16396, counted(402653184)),
		indexSizeRow(16397, counted(207618048)),
		indexSizeRow(16487, counted(99999744)),
		indexSizeRow(16488, counted(92512256)),
	}
}

type fakeIndexUsageConn struct {
	*fakeWindowConn

	stats []fakeResult
	sizes []fakeResult

	statsArgs [][]any
	sizesArgs [][]any
}

func newFakeIndexUsageConn() *fakeIndexUsageConn {
	return &fakeIndexUsageConn{
		fakeWindowConn: newFakeWindowConn(),
		stats:          queue(rowsResult(ordersIndexesStart()), rowsResult(ordersIndexesEnd())),
		sizes:          repeat(rowsResult(ordersIndexSizes())),
	}
}

func (c *fakeIndexUsageConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch sql {
	case indexUsageStatsSQL:
		c.statsArgs = append(c.statsArgs, args)
		return answer(&c.stats)

	case indexUsageSizesSQL:
		c.sizesArgs = append(c.sizesArgs, args)
		return answer(&c.sizes)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func runIndexUsageWindow(t *testing.T, clock *scriptedClock, target Target,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     target,
		Duration:   120 * time.Second,
		Collectors: []Collector{IndexUsage{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func indexUsageRows(t *testing.T, block string) [][]string {
	t.Helper()

	var body strings.Builder
	for line := range strings.SplitSeq(block, "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}

	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)
	require.Equal(t, indexUsageColumns, records[0], "the column header leads every block")

	return records[1:]
}

func takeIndexSample(t *testing.T, conn *fakeIndexUsageConn, collector IndexUsage) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestIndexUsageArtifact(t *testing.T) {
	artifact := IndexUsage{}.Artifact()

	assert.Equal(t, "pg_index_usage", artifact.Name)
	assert.Equal(t, "pg_index_usage.txt", artifact.FileName)
	assert.Equal(t, "database", artifact.Scope,
		"pg_stat_user_indexes shows the connected database's indexes only, like pg_bloat.txt's view")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), IndexUsage{Interval: 15 * time.Second}.Artifact().Schedule,
		"born periodic: the run's cadence, with the close as the last sample")
	assert.Zero(t, artifact.SampleBudget, "two statements is DefaultSampleBudget already")
}

func TestIndexUsageColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"indexrelid",
		"relid",
		"indexrelname",
		"idx_scan",
		"idx_tup_read",
		"idx_tup_fetch",
		"index_size_bytes",
	}, indexUsageColumns, "the column list, in this order")

	assert.Equal(t, "indexrelid", indexUsageColumns[colIndexrelid], "the join key leads")
}

func TestIndexUsageCopiesBloatsTwoStatementShape(t *testing.T) {
	assert.NotContains(t, indexUsageStatsSQL, "pg_relation_size",
		"the size function never shares a statement with the counters: a large schema's "+
			"file stats could time the statement out and take the scan counts with them")
	assert.Contains(t, indexUsageStatsSQL, "ORDER BY indexrelid",
		"determinism, never a statistic, so two samples cap on the same index set")

	assert.Contains(t, indexUsageSizesSQL, "unnest($1::oid[])",
		"S2 sizes exactly the OIDs S1 returned, not a second view scan")
	assert.Contains(t, indexUsageSizesSQL, "LEFT JOIN pg_catalog.pg_class",
		"an index dropped between the two statements yields a NULL size rather than a "+
			"vanished OID reaching pg_relation_size")
}

func TestIndexUsageGoldenFull(t *testing.T) {
	results := runIndexUsageWindow(t, goldenClock(t), testTarget(),
		connectTo(newFakeIndexUsageConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_index_usage_full.txt"), artifactText(t, results[0]))
}

func TestIndexUsageGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runIndexUsageWindow(t, clock, testTarget(),
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_index_usage_connect_failure.txt"), artifactText(t, results[0]))
}

func TestIndexUsageGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeIndexUsageConn()
	conn.stats = queue(
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(ordersIndexesEnd()),
	)

	results := runIndexUsageWindow(t, clock, testTarget(), connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_index_usage_sample_error.txt"), artifactText(t, results[0]))
}

func TestIndexUsageGoldenEmptyDatabase(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult(nil))
	conn.database = "postgres"

	target := testTarget()
	target.Database = "postgres"

	results := runIndexUsageWindow(t, goldenClock(t), target, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"the capture worked and there was nothing to capture")
	assert.Equal(t, bloatGolden(t, "pg_index_usage_empty_db.txt"), artifactText(t, results[0]))
}

func TestIndexUsageNeverScannedIsZeroNotEmpty(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult([][]any{
		indexRow(16397, 16390, "orders_status_idx", counted(0), counted(0), counted(0), 2),
		indexRow(16398, 16390, "no_statistics_row", nil, nil, nil, 2),
	}))
	conn.sizes = repeat(rowsResult([][]any{
		indexSizeRow(16397, counted(8192)),
		indexSizeRow(16398, counted(0)),
	}))

	rows := indexUsageRows(t, takeIndexSample(t, conn, IndexUsage{}))
	require.Len(t, rows, 2)

	assert.Equal(t, "0", rows[0][colIndexScan],
		"an index nothing scans is the finding, and must survive as 0")
	assert.Equal(t, "0", rows[0][colIndexTupRead])
	assert.Equal(t, "0", rows[0][colIndexTupFetch])

	assert.Equal(t, "", rows[1][colIndexScan],
		"a NULL the view is not expected to produce still renders empty, never as a 0 reading")
	assert.Equal(t, "", rows[1][colIndexTupRead])
	assert.Equal(t, "", rows[1][colIndexTupFetch])

	assert.Equal(t, "8192", rows[0][colIndexSizeBytes])
	assert.Equal(t, "0", rows[1][colIndexSizeBytes],
		"no storage is 0; empty stays reserved for an index that vanished")
}

func TestIndexUsageIndexMissingFromTheSizeJoinHasAnEmptySize(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult([][]any{
		indexRow(16396, 16390, "orders_pkey", counted(1), counted(1), counted(1), 2),
		indexRow(16397, 16390, "dropped_mid_sample", counted(1), counted(1), counted(1), 2),
	}))
	conn.sizes = repeat(rowsResult([][]any{
		indexSizeRow(16396, counted(8192)),
		indexSizeRow(16397, nil),
	}))

	rows := indexUsageRows(t, takeIndexSample(t, conn, IndexUsage{}))
	require.Len(t, rows, 2)

	assert.Equal(t, "8192", rows[0][colIndexSizeBytes])
	assert.Equal(t, "", rows[1][colIndexSizeBytes],
		"a vanished index has no size, which is not the same as zero")
}

func TestIndexUsageSizesUnavailableCostsOneColumnNotTheSample(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"statement timeout", errors.New("ERROR: canceling statement due to statement timeout")},
		{"module deadline mid-S2", context.DeadlineExceeded},
		{"cancellation mid-S2", context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := newFakeIndexUsageConn()
			conn.sizes = repeat(errResult(tt.err))

			block := takeIndexSample(t, conn, IndexUsage{})

			assert.Contains(t, block, "sizes=unavailable")
			assert.Contains(t, block, "indexes_written=4 indexes_total=4 truncated=false",
				"the sample is still written and still counted")

			for _, row := range indexUsageRows(t, block) {
				assert.Equal(t, "", row[colIndexSizeBytes], "the size column is empty on every row")
				assert.NotEmpty(t, row[colIndexScan], "the scan counts survive")
			}
		})
	}
}

func TestIndexUsageSizesUnavailableReasonIsQuotedInTheHeader(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.sizes = repeat(errResult(
		errors.New("ERROR: canceling statement due to statement timeout")))

	assert.Contains(t, takeIndexSample(t, conn, IndexUsage{}),
		`sizes=unavailable reason="ERROR: canceling statement due to statement timeout"`)
}

func TestIndexUsageSizesUnavailableReasonIsRedacted(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.sizes = repeat(errResult(fmt.Errorf("dial failed for %s", testPassword)))

	results := runIndexUsageWindow(t, goldenClock(t), testTarget(), connectTo(conn))

	artifact := artifactText(t, results[0])
	assert.NotContains(t, artifact, testPassword)
	assert.Contains(t, artifact, "<redacted>")
}

func TestIndexUsageFailingStatsWritesNothing(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(errResult(
		errors.New("ERROR: permission denied for view pg_stat_user_indexes")))

	var buf bytes.Buffer
	err := IndexUsage{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched")
	assert.Empty(t, conn.sizesArgs, "and does not go on to run the expensive half")
}

func TestIndexUsageWritesTheBlockInOneWrite(t *testing.T) {
	conn := newFakeIndexUsageConn()
	writer := &countingWriter{}

	require.NoError(t, IndexUsage{}.Sample(context.Background(), conn, writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestIndexUsageCapFiresVisibly(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult([][]any{
		indexRow(16396, 16390, "orders_pkey", counted(1), counted(1), counted(1), 41220),
	}))
	conn.sizes = repeat(rowsResult([][]any{indexSizeRow(16396, counted(8192))}))

	assert.Contains(t, takeIndexSample(t, conn, IndexUsage{MaxIndexes: 1}),
		"indexes_written=1 indexes_total=41220 truncated=true",
		"a capped file must not read as a complete one")

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, []any{1}, conn.statsArgs[0], "the cap is the LIMIT the server is sent")
}

func TestIndexUsageDefaultCapIsSentWhenUnset(t *testing.T) {
	conn := newFakeIndexUsageConn()

	takeIndexSample(t, conn, IndexUsage{})

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, []any{DefaultMaxIndexes}, conn.statsArgs[0])
	assert.Equal(t, 2*DefaultMaxTables, DefaultMaxIndexes,
		"indexes outnumber tables: a schema at bloat's cap with two indexes per table still fits")
}

func TestIndexUsageSizesQueryTakesExactlyTheStatsIndexrelids(t *testing.T) {
	conn := newFakeIndexUsageConn()

	takeIndexSample(t, conn, IndexUsage{})

	require.Len(t, conn.sizesArgs, 1)
	assert.Equal(t, []any{[]uint32{16396, 16397, 16487, 16488}}, conn.sizesArgs[0],
		"pgx encodes []uint32 as oid[], which bloat's live check confirmed")
}

func TestIndexUsageEmptyDatabaseSkipsTheSizeQuery(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult(nil))

	block := takeIndexSample(t, conn, IndexUsage{})

	assert.Contains(t, block, "indexes_written=0 indexes_total=0 truncated=false")
	assert.Empty(t, conn.sizesArgs, "there is nothing to size")
	assert.Empty(t, indexUsageRows(t, block), "the column header is written with no rows under it")
}

func TestIndexUsageIdentifiersWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeIndexUsageConn()
	conn.stats = repeat(rowsResult([][]any{
		indexRow(16396, 16390, "line\nbreak,\"quoted\"", counted(1), counted(2), counted(3), 1),
	}))
	conn.sizes = repeat(rowsResult([][]any{indexSizeRow(16396, counted(4))}))

	block := takeIndexSample(t, conn, IndexUsage{})

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	rows := indexUsageRows(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "line break,\"quoted\"", rows[0][colIndexRelName],
		"the line break is flattened to a space; the comma and quotes survive CSV quoting")
}
