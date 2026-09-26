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

func classRow(oid uint32, name string, namespace uint32, kind string) []any {
	return []any{oid, name, namespace, kind}
}

func nameRow(oid uint32, name string) []any {
	return []any{oid, name}
}

// In the catalog's own order, which is no order: a partitioned table with its
// two partitions and a materialized view in a schema of their own, beside the
// system catalogs every database has.
func catalogClasses() [][]any {
	return [][]any{
		classRow(16412, "orders", 2200, "r"),
		classRow(16419, "orders_pkey", 2200, "i"),
		classRow(16421, "orders_customer_id_idx", 2200, "i"),
		classRow(16430, "order_items", 2200, "r"),
		classRow(16437, "order_items_pkey", 2200, "i"),
		classRow(16450, "invoices", 16410, "p"),
		classRow(16455, "invoices_2026_07", 16410, "r"),
		classRow(16460, "invoices_2026_08", 16410, "r"),
		classRow(16470, "daily_revenue", 16410, "m"),
		classRow(2619, "pg_statistic", 11, "r"),
		classRow(1247, "pg_type", 11, "r"),
		classRow(2703, "pg_type_oid_index", 11, "i"),
		classRow(1259, "pg_class", 11, "r"),
		classRow(2662, "pg_class_oid_index", 11, "i"),
	}
}

func catalogNamespaces() [][]any {
	return [][]any{
		nameRow(99, "pg_toast"),
		nameRow(11, "pg_catalog"),
		nameRow(2200, "public"),
		nameRow(13283, "information_schema"),
		nameRow(16410, "billing"),
	}
}

func catalogDatabases() [][]any {
	return [][]any{
		nameRow(5, "postgres"),
		nameRow(16401, "orders_db"),
		nameRow(1, "template1"),
		nameRow(4, "template0"),
	}
}

type fakeCatalogMapConn struct {
	*fakeWindowConn

	results map[string][]fakeResult
	queries []string
}

func newFakeCatalogMapConn() *fakeCatalogMapConn {
	return &fakeCatalogMapConn{
		fakeWindowConn: newFakeWindowConn(),
		results: map[string][]fakeResult{
			catalogClassSQL:     repeat(rowsResult(catalogClasses())),
			catalogNamespaceSQL: repeat(rowsResult(catalogNamespaces())),
			catalogDatabaseSQL:  repeat(rowsResult(catalogDatabases())),
		},
	}
}

func (c *fakeCatalogMapConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	pending, ok := c.results[sql]
	if !ok {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	if len(args) != 0 {
		return nil, fmt.Errorf("unexpected arguments: %v", args)
	}

	c.queries = append(c.queries, sql)

	rows, err := answer(&pending)
	c.results[sql] = pending

	return rows, err
}

// failingPartwayConn returns every row of one statement, then an error: a
// result that breaks off after the rows were read.
type failingPartwayConn struct {
	*fakeCatalogMapConn

	sql string
}

func (c *failingPartwayConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql == c.sql {
		return &fakeRows{values: catalogClasses(), err: errors.New("unexpected EOF")}, nil
	}

	return c.fakeCatalogMapConn.Query(ctx, sql, args...)
}

func runCatalogMapWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{CatalogMap{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

// onceClock is the preamble, the timeline's start and its one tick, the sample,
// and the close.
func onceClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 112),
		at(32, 5, 131),
	)
}

type catalogMapBlock struct {
	header  map[string]string
	columns []string
	rows    [][]string
}

// catalogMapBlocks parses the blocks a sample wrote, in order.
func catalogMapBlocks(t *testing.T, sample string) []catalogMapBlock {
	t.Helper()

	var blocks []catalogMapBlock

	for line := range strings.SplitSeq(strings.TrimSuffix(sample, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			blocks = append(blocks, catalogMapBlock{header: headerFields(t, line)})
			continue
		}

		require.NotEmpty(t, blocks, "a body line before any header: %s", line)

		record, err := csv.NewReader(strings.NewReader(line)).Read()
		require.NoError(t, err, "not a CSV record: %s", line)

		current := &blocks[len(blocks)-1]
		if current.columns == nil {
			current.columns = record
			continue
		}

		current.rows = append(current.rows, record)
	}

	return blocks
}

func takeCatalogMapSample(t *testing.T, conn RowQuerier) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, CatalogMap{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestCatalogMapArtifact(t *testing.T) {
	artifact := CatalogMap{}.Artifact()

	assert.Equal(t, "pg_catalog_map", artifact.Name)
	assert.Equal(t, "pg_catalog_map.txt", artifact.FileName)
	assert.Equal(t, "database", artifact.Scope, "most of what it names is the connected database's")
	assert.Equal(t, Once(), artifact.Schedule, "read at the start and not resampled")
	assert.Equal(t, 3*StatementTimeout, artifact.SampleBudget, "three statements, declared")
	assert.Equal(t, []time.Duration{0}, artifact.Schedule.offsets(120*time.Second),
		"one sample, at the start")
}

func TestCatalogMapStatementsReadEveryRowALockCanName(t *testing.T) {
	assert.Contains(t, catalogClassSQL, "SELECT oid, relname::text, relnamespace, relkind::text")
	assert.Contains(t, catalogClassSQL, "FROM pg_catalog.pg_class")
	assert.Contains(t, catalogClassSQL, "WHERE relkind IN ('r', 'i', 'm', 'p')",
		"tables, indexes, materialized views and partitioned tables")
	assert.Contains(t, catalogNamespaceSQL, "SELECT oid, nspname::text")
	assert.Contains(t, catalogNamespaceSQL, "FROM pg_catalog.pg_namespace")
	assert.Contains(t, catalogDatabaseSQL, "SELECT oid, datname::text")
	assert.Contains(t, catalogDatabaseSQL, "FROM pg_catalog.pg_database")

	for _, sql := range []string{catalogClassSQL, catalogNamespaceSQL, catalogDatabaseSQL} {
		assert.NotContains(t, sql, "ORDER BY", "no order: %s", sql)
		assert.NotContains(t, sql, "LIMIT", "no cap: %s", sql)
	}

	assert.NotContains(t, catalogNamespaceSQL, "WHERE", "every schema")
	assert.NotContains(t, catalogDatabaseSQL, "WHERE", "every database")
}

func TestCatalogMapWritesThreeBlocksInOrder(t *testing.T) {
	conn := newFakeCatalogMapConn()
	blocks := catalogMapBlocks(t, takeCatalogMapSample(t, conn))
	require.Len(t, blocks, 3)

	assert.Equal(t, []string{catalogClassSQL, catalogNamespaceSQL, catalogDatabaseSQL}, conn.queries,
		"one statement per block, each once")

	for i, want := range []struct {
		source, scope string
		columns       []string
		rows          int
	}{
		{"pg_class", "database", []string{"oid", "relname", "relnamespace", "relkind"}, 14},
		{"pg_namespace", "database", []string{"oid", "nspname"}, 5},
		{"pg_database", "cluster", []string{"oid", "datname"}, 4},
	} {
		assert.Equal(t, want.source, blocks[i].header["source"], "block %d", i)
		assert.Equal(t, want.scope, blocks[i].header["scope"], "block %d", i)
		assert.Equal(t, "orders_db", blocks[i].header["db"], "block %d", i)
		assert.Equal(t, "16401", blocks[i].header["dbid"], "block %d", i)
		assert.Equal(t, "1", blocks[i].header["sample"], "block %d", i)
		assert.Equal(t, want.columns, blocks[i].columns, "block %d", i)
		assert.Len(t, blocks[i].rows, want.rows, "block %d", i)
		assert.NotContains(t, blocks[i].header, "error", "block %d", i)
	}

	assert.Equal(t, []string{"16450", "invoices", "16410", "p"}, blocks[0].rows[5],
		"the partitioned parent, which is what a lock on a partitioned table names")
	assert.Equal(t, []string{"16410", "billing"}, blocks[1].rows[4], "its namespace, resolved by oid")
	assert.Equal(t, []string{"4", "template0"}, blocks[2].rows[3], "every database, template0 too")
}

func TestCatalogMapScopeSaysWhichCatalogsAreTheConnectedDatabasesOwn(t *testing.T) {
	for _, block := range catalogBlocks {
		switch block.source {
		case "pg_class", "pg_namespace":
			assert.Equal(t, "database", block.scope,
				"%s names the connected database's objects only: a lock elsewhere keeps its OIDs", block.source)

		default:
			assert.Equal(t, "cluster", block.scope, "%s is shared by every database", block.source)
		}
	}
}

func TestCatalogMapGoldenFull(t *testing.T) {
	results := runCatalogMapWindow(t, onceClock(t), connectTo(newFakeCatalogMapConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_catalog_map_full.txt"), artifactText(t, results[0]))
}

func TestCatalogMapGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runCatalogMapWindow(t, clock,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_catalog_map_connect_failure.txt"), artifactText(t, results[0]))
}

func TestCatalogMapGoldenBlockError(t *testing.T) {
	conn := newFakeCatalogMapConn()
	conn.results[catalogClassSQL] = repeat(errResult(errors.New("ERROR: canceling statement due to statement timeout")))

	results := runCatalogMapWindow(t, onceClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"a refused block is recorded inside a written sample, as in pg_capacity.txt")
	assert.Equal(t, bloatGolden(t, "pg_catalog_map_block_error.txt"), artifactText(t, results[0]))
}

func TestCatalogMapAFailedReadIsItsOwnBlocksError(t *testing.T) {
	for i, failing := range catalogBlocks {
		t.Run(failing.source, func(t *testing.T) {
			conn := newFakeCatalogMapConn()
			conn.results[failing.sql] = repeat(errResult(errors.New("ERROR: canceling statement due to statement timeout")))

			blocks := catalogMapBlocks(t, takeCatalogMapSample(t, conn))
			require.Len(t, blocks, 3, "every block is written")

			for n, block := range blocks {
				if n != i {
					assert.NotContains(t, block.header, "error", "%s", block.header["source"])
					assert.NotEmpty(t, block.rows, "%s: the other reads are kept", block.header["source"])

					continue
				}

				assert.Equal(t, "ERROR: canceling statement due to statement timeout", block.header["error"])
				assert.Equal(t, failing.columns, block.columns, "the column header is still written")
				assert.Empty(t, block.rows, "and no rows")
			}
		})
	}
}

func TestCatalogMapAResultBrokenOffPartwayWritesNoRows(t *testing.T) {
	conn := &failingPartwayConn{fakeCatalogMapConn: newFakeCatalogMapConn(), sql: catalogClassSQL}

	blocks := catalogMapBlocks(t, takeCatalogMapSample(t, conn))
	require.Len(t, blocks, 3)

	assert.Equal(t, "unexpected EOF", blocks[0].header["error"])
	assert.Empty(t, blocks[0].rows, "every row or none: a partial map would pass for a whole one")
	assert.NotEmpty(t, blocks[1].rows)
}

func TestCatalogMapWritesTheSampleInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, CatalogMap{}.Sample(context.Background(), newFakeCatalogMapConn(), writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes, "a write failing between blocks would leave a partial map behind")
	assert.NotEmpty(t, writer.buf.String())
}

func TestCatalogMapNamesWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeCatalogMapConn()
	conn.results[catalogClassSQL] = repeat(rowsResult([][]any{classRow(16412, "line\nbreak,\"quoted\"", 2200, "r")}))

	blocks := catalogMapBlocks(t, takeCatalogMapSample(t, conn))
	require.Len(t, blocks[0].rows, 1, "exactly one data line")

	assert.Equal(t, []string{"16412", "line break,\"quoted\"", "2200", "r"}, blocks[0].rows[0],
		"the line break is flattened to a space; the comma and quotes survive CSV quoting")
}
