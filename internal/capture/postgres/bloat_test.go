package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colRelid = iota
	colSchemaName
	colRelName
	colLiveTup
	colDeadTup
	colTupUpd
	colTupHotUpd
	colSeqScan
	colIdxScan
	colLastAutovacuum
	colLastVacuum
	colTableSize
	colIndexSize
)

var (
	testOrdersVacuum = time.Date(2026, 7, 25, 13, 50, 0, 0, time.UTC)
	testItemsVacuum  = time.Date(2026, 7, 25, 8, 41, 0, 0, time.UTC)
)

var testdataDir = func() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}

	return filepath.Join(dir, "testdata")
}()

type fakeRows struct {
	values [][]any
	err    error

	index int
}

func (r *fakeRows) Next() bool {
	if r.index >= len(r.values) {
		return false
	}

	r.index++

	return true
}

func (r *fakeRows) Scan(dest ...any) error {
	row := r.values[r.index-1]

	if len(dest) != len(row) {
		return fmt.Errorf("scan into %d destinations from %d values", len(dest), len(row))
	}

	for i := range dest {
		if err := assign(dest[i], row[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}

	return nil
}

func (r *fakeRows) Err() error                                   { return r.err }
func (r *fakeRows) Close()                                       {}
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakeRows) RawValues() [][]byte                          { return nil }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }

func statsRow(relid uint32, schema, name string, live, dead, upd, hot, seq int64, idx *int64,
	autovacuum, vacuum *time.Time, total int64,
) []any {
	return []any{
		relid, schema, name,
		ptr(live), ptr(dead), ptr(upd), ptr(hot), ptr(seq), idx,
		autovacuum, vacuum, total,
	}
}

func sizeRow(relid uint32, table, index *int64) []any {
	return []any{relid, table, index}
}

func ordersSampleStart() [][]any {
	return [][]any{
		statsRow(16390, "public", "orders", 4210044, 412988, 884213, 689882, 88104,
			ptr(int64(4021884)), &testOrdersVacuum, &testOrdersVacuum, 2),
		statsRow(16482, "public", "orders_line_items", 912004, 458210, 221904, 90981, 340120,
			ptr(int64(1200440)), &testItemsVacuum, &testItemsVacuum, 2),
	}
}

func ordersSampleEnd() [][]any {
	return [][]any{
		statsRow(16390, "public", "orders", 4211200, 413410, 884340, 689951, 88220,
			ptr(int64(4025901)), &testOrdersVacuum, &testOrdersVacuum, 2),
		statsRow(16482, "public", "orders_line_items", 912340, 466100, 221990, 91040, 340460,
			ptr(int64(1201980)), &testItemsVacuum, &testItemsVacuum, 2),
	}
}

func ordersSizes() [][]any {
	return [][]any{
		sizeRow(16390, ptr(int64(2415919104)), ptr(int64(610271232))),
		sizeRow(16482, ptr(int64(884736000)), ptr(int64(192512000))),
	}
}

type fakeResult struct {
	rows [][]any
	err  error
}

func rowsResult(rows [][]any) fakeResult { return fakeResult{rows: rows} }
func errResult(err error) fakeResult     { return fakeResult{err: err} }
func repeat(r fakeResult) []fakeResult   { return []fakeResult{r} }
func queue(r ...fakeResult) []fakeResult { return r }

type fakeBloatConn struct {
	*fakeWindowConn

	stats []fakeResult
	sizes []fakeResult

	statsArgs [][]any
	sizesArgs [][]any
}

func newFakeBloatConn() *fakeBloatConn {
	return &fakeBloatConn{
		fakeWindowConn: newFakeWindowConn(),
		stats:          queue(rowsResult(ordersSampleStart()), rowsResult(ordersSampleEnd())),
		sizes:          repeat(rowsResult(ordersSizes())),
	}
}

func (c *fakeBloatConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch sql {
	case bloatStatsSQL:
		c.statsArgs = append(c.statsArgs, args)
		return answer(&c.stats)

	case bloatSizesSQL:
		c.sizesArgs = append(c.sizesArgs, args)
		return answer(&c.sizes)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func answer(pending *[]fakeResult) (pgx.Rows, error) {
	if len(*pending) == 0 {
		return &fakeRows{}, nil
	}

	head := (*pending)[0]
	if len(*pending) > 1 {
		*pending = (*pending)[1:]
	}

	if head.err != nil {
		return nil, head.err
	}

	return &fakeRows{values: head.rows}, nil
}

type scriptedClock struct {
	t     *testing.T
	times []time.Time
	index int
}

func newScriptedClock(t *testing.T, times ...time.Time) *scriptedClock {
	t.Helper()

	return &scriptedClock{t: t, times: times}
}

func (c *scriptedClock) now() time.Time {
	c.t.Helper()
	require.Less(c.t, c.index, len(c.times), "scripted clock exhausted after %d reads", c.index)

	next := c.times[c.index]
	c.index++

	return next
}

func (c *scriptedClock) after(time.Duration) <-chan time.Time {
	fired := make(chan time.Time, 1)
	fired <- time.Time{}

	return fired
}

func at(minute, second, milli int) time.Time {
	return time.Date(2026, 8, 7, 14, minute, second, milli*int(time.Millisecond), time.UTC)
}

func goldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 112),
		at(32, 5, 112),
		at(34, 5, 140),
		at(34, 5, 201),
	)
}

func runBloatWindow(t *testing.T, clock *scriptedClock, target Target, collector Bloat,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     target,
		Duration:   120 * time.Second,
		Collectors: []Collector{collector},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func connectTo(conn windowConn) func(context.Context, Target) (windowConn, error) {
	return func(context.Context, Target) (windowConn, error) { return conn, nil }
}

func bloatGolden(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(testdataDir, name))
	require.NoError(t, err)

	return string(content)
}

func sampleRows(t *testing.T, block string) [][]string {
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
	require.Equal(t, bloatColumns, records[0], "the column header leads every block")

	return records[1:]
}

func takeSample(t *testing.T, conn *fakeBloatConn, collector Bloat) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestBloatArtifact(t *testing.T) {
	artifact := Bloat{}.Artifact()

	assert.Equal(t, "pg_bloat", artifact.Name)
	assert.Equal(t, "pg_bloat.txt", artifact.FileName)
	assert.Equal(t, "database", artifact.Scope)
	assert.Equal(t, ScheduleStartEnd, artifact.Schedule)
}

func TestBloatColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"relid",
		"schemaname",
		"relname",
		"n_live_tup",
		"n_dead_tup",
		"n_tup_upd",
		"n_tup_hot_upd",
		"seq_scan",
		"idx_scan",
		"last_autovacuum",
		"last_vacuum",
		"table_size_bytes",
		"index_size_bytes",
	}, bloatColumns)

	assert.Equal(t, "relid", bloatColumns[colRelid], "the join key leads")
}

func TestBloatGoldenFull(t *testing.T) {
	results := runBloatWindow(t, goldenClock(t), testTarget(), Bloat{},
		connectTo(newFakeBloatConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_bloat_full.txt"), artifactText(t, results[0]))
}

func TestBloatGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runBloatWindow(t, clock, testTarget(), Bloat{},
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_bloat_connect_failure.txt"), artifactText(t, results[0]))
}

func TestBloatGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeBloatConn()
	conn.stats = queue(
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(ordersSampleEnd()),
	)

	results := runBloatWindow(t, clock, testTarget(), Bloat{}, connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_bloat_sample_error.txt"), artifactText(t, results[0]))
}

func TestBloatGoldenEmptyDatabase(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult(nil))
	conn.database = "postgres"

	target := testTarget()
	target.Database = "postgres"

	results := runBloatWindow(t, goldenClock(t), target, Bloat{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"the capture worked and there was nothing to capture")
	assert.Equal(t, bloatGolden(t, "pg_bloat_empty_db.txt"), artifactText(t, results[0]))
}

func TestBloatWritesNullsEmptyNeverZero(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult([][]any{
		statsRow(16390, "public", "no_indexes", 100, 0, 0, 0, 4, nil, nil, nil, 2),
		statsRow(16482, "public", "never_scanned", 100, 0, 0, 0, 4, ptr(int64(0)),
			&testOrdersVacuum, nil, 2),
	}))
	conn.sizes = repeat(rowsResult([][]any{sizeRow(16390, ptr(int64(8192)), ptr(int64(0)))}))

	rows := sampleRows(t, takeSample(t, conn, Bloat{}))
	require.Len(t, rows, 2)

	assert.Equal(t, "", rows[0][colIdxScan], "a table with no indexes reports NULL, not 0")
	assert.Equal(t, "0", rows[1][colIdxScan], "an unused index is a finding and must survive as 0")

	assert.Equal(t, "", rows[0][colLastAutovacuum], "never autovacuumed is empty, not an epoch")
	assert.Equal(t, "", rows[0][colLastVacuum])
	assert.Equal(t, "2026-07-25T13:50:00.000Z", rows[1][colLastAutovacuum])
	assert.Equal(t, "", rows[1][colLastVacuum])

	assert.Equal(t, "8192", rows[0][colTableSize])
	assert.Equal(t, "0", rows[0][colIndexSize])
}

func TestBloatRelationMissingFromTheSizeJoinHasEmptySizes(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult([][]any{
		statsRow(16390, "public", "orders", 100, 0, 0, 0, 4, ptr(int64(1)),
			&testOrdersVacuum, &testOrdersVacuum, 2),
		statsRow(16482, "public", "dropped_mid_sample", 100, 0, 0, 0, 4, ptr(int64(1)),
			&testOrdersVacuum, &testOrdersVacuum, 2),
	}))
	conn.sizes = repeat(rowsResult([][]any{
		sizeRow(16390, ptr(int64(8192)), ptr(int64(4096))),
		sizeRow(16482, nil, nil),
	}))

	rows := sampleRows(t, takeSample(t, conn, Bloat{}))
	require.Len(t, rows, 2)

	assert.Equal(t, []string{"8192", "4096"}, rows[0][colTableSize:])
	assert.Equal(t, []string{"", ""}, rows[1][colTableSize:],
		"a vanished relation has no size, which is not the same as zero")
}

func TestBloatSizesUnavailableCostsTwoColumnsNotTheSample(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{"statement timeout", errors.New("ERROR: canceling statement due to statement timeout")},
		{"module deadline mid-S2", context.DeadlineExceeded},
		{"cancellation mid-S2", context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			conn := newFakeBloatConn()
			conn.sizes = repeat(errResult(tt.err))

			block := takeSample(t, conn, Bloat{})

			assert.Contains(t, block, "sizes=unavailable")
			assert.Contains(t, block, "tables_written=2 tables_total=2 truncated=false",
				"the sample is still written and still counted")

			for _, row := range sampleRows(t, block) {
				assert.Equal(t, []string{"", ""}, row[colTableSize:],
					"both size columns are empty across every row")
				assert.NotEmpty(t, row[colDeadTup], "the dead-tuple counts survive")
			}
		})
	}
}

func TestBloatSizesUnavailableReasonIsQuotedInTheHeader(t *testing.T) {
	conn := newFakeBloatConn()
	conn.sizes = repeat(errResult(
		errors.New("ERROR: canceling statement due to statement timeout")))

	assert.Contains(t, takeSample(t, conn, Bloat{}),
		`sizes=unavailable reason="ERROR: canceling statement due to statement timeout"`)
}

func TestBloatSizesUnavailableReasonIsRedacted(t *testing.T) {
	conn := newFakeBloatConn()
	conn.sizes = repeat(errResult(fmt.Errorf("dial failed for %s", testPassword)))

	results := runBloatWindow(t, goldenClock(t), testTarget(), Bloat{}, connectTo(conn))

	artifact := artifactText(t, results[0])
	assert.NotContains(t, artifact, testPassword)
	assert.Contains(t, artifact, "<redacted>")
}

func TestBloatFailingStatsWritesNothing(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(errResult(
		errors.New("ERROR: permission denied for view pg_stat_user_tables")))

	var buf bytes.Buffer
	err := Bloat{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched")
	assert.Empty(t, conn.sizesArgs, "and does not go on to run the expensive half")
}

type countingWriter struct {
	writes int
	buf    bytes.Buffer
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.writes++

	return w.buf.Write(p)
}

func TestBloatWritesTheBlockInOneWrite(t *testing.T) {
	conn := newFakeBloatConn()
	writer := &countingWriter{}

	require.NoError(t, Bloat{}.Sample(context.Background(), conn, writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestBloatCapFiresVisibly(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult([][]any{
		statsRow(16390, "public", "orders", 100, 1, 1, 1, 1, ptr(int64(1)),
			&testOrdersVacuum, &testOrdersVacuum, 41220),
	}))
	conn.sizes = repeat(rowsResult([][]any{sizeRow(16390, ptr(int64(8192)), ptr(int64(4096)))}))

	assert.Contains(t, takeSample(t, conn, Bloat{MaxTables: 1}),
		"tables_written=1 tables_total=41220 truncated=true",
		"a capped file must not read as a complete one")

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, []any{1}, conn.statsArgs[0], "the cap is the LIMIT the server is sent")
}

func TestBloatDefaultCapIsSentWhenUnset(t *testing.T) {
	conn := newFakeBloatConn()

	takeSample(t, conn, Bloat{})

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, []any{DefaultMaxTables}, conn.statsArgs[0])
}

func TestBloatSizesQueryTakesExactlyTheStatsRelids(t *testing.T) {
	conn := newFakeBloatConn()

	takeSample(t, conn, Bloat{})

	require.Len(t, conn.sizesArgs, 1)
	assert.Equal(t, []any{[]uint32{16390, 16482}}, conn.sizesArgs[0],
		"pgx encodes []uint32 as oid[], which the live check confirmed")
}

func TestBloatEmptyDatabaseSkipsTheSizeQuery(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult(nil))

	block := takeSample(t, conn, Bloat{})

	assert.Contains(t, block, "tables_written=0 tables_total=0 truncated=false")
	assert.Empty(t, conn.sizesArgs, "there is nothing to size")
	assert.Empty(t, sampleRows(t, block), "the column header is written with no rows under it")
}

func TestBloatIdentifiersWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeBloatConn()
	conn.stats = repeat(rowsResult([][]any{
		statsRow(16390, "we,ird\"schema", "line\nbreak,\"quoted\"", 1, 2, 3, 4, 5,
			ptr(int64(6)), &testOrdersVacuum, &testOrdersVacuum, 1),
	}))
	conn.sizes = repeat(rowsResult([][]any{sizeRow(16390, ptr(int64(1)), ptr(int64(2)))}))

	block := takeSample(t, conn, Bloat{})

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	rows := sampleRows(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "we,ird\"schema", rows[0][colSchemaName])
	assert.Equal(t, "line break,\"quoted\"", rows[0][colRelName],
		"the line break is flattened to a space; the comma and quotes survive CSV quoting")
}
