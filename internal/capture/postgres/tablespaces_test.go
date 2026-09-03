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
	colSpcName = iota
	colSpcSize
)

func sizeRowFor(name string, size *int64) []any {
	return []any{name, size}
}

// The cluster pg_metadata_full.txt describes: its one tablespace with storage of
// its own, and the two every cluster has. pg_default holds every database, so it
// is the row that moves between the samples; pg_global is the shared catalogues.
// Sorted by name, which is the statement's order.
func tablespaceSizesStart() [][]any {
	return [][]any{
		sizeRowFor("orders_archive", counted(52428800)),
		sizeRowFor("pg_default", counted(3617382400)),
		sizeRowFor("pg_global", counted(622592)),
	}
}

func tablespaceSizesEnd() [][]any {
	return [][]any{
		sizeRowFor("orders_archive", counted(52428800)),
		sizeRowFor("pg_default", counted(3617644544)),
		sizeRowFor("pg_global", counted(622592)),
	}
}

// A LOGIN-only role: the database's default tablespace is readable without a
// grant, the other two are not, and the guard turns each refusal into an empty
// cell rather than a failed statement.
func tablespaceSizesLeastPrivilege() [][]any {
	return [][]any{
		sizeRowFor("orders_archive", nil),
		sizeRowFor("pg_default", counted(3617382400)),
		sizeRowFor("pg_global", nil),
	}
}

type fakeTablespacesConn struct {
	*fakeWindowConn

	sizes []fakeResult
	args  [][]any
}

func newFakeTablespacesConn() *fakeTablespacesConn {
	return &fakeTablespacesConn{
		fakeWindowConn: newFakeWindowConn(),
		sizes:          queue(rowsResult(tablespaceSizesStart()), rowsResult(tablespaceSizesEnd())),
	}
}

func (c *fakeTablespacesConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != tablespaceSizesSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	c.args = append(c.args, args)

	return answer(&c.sizes)
}

func runTablespacesWindow(t *testing.T, clock *scriptedClock, target Target,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     target,
		Duration:   120 * time.Second,
		Collectors: []Collector{Tablespaces{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func tablespaceRows(t *testing.T, block string) [][]string {
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
	require.Equal(t, tablespaceColumns, records[0], "the column header leads every block")

	return records[1:]
}

func takeTablespaceSample(t *testing.T, conn *fakeTablespacesConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, Tablespaces{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestTablespacesArtifact(t *testing.T) {
	artifact := Tablespaces{}.Artifact()

	assert.Equal(t, "pg_tablespaces", artifact.Name)
	assert.Equal(t, "pg_tablespaces.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"tablespaces belong to the cluster: db= and dbid= mean connected through, not about")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), Tablespaces{Interval: 15 * time.Second}.Artifact().Schedule,
		"born periodic: the run's cadence, with the close as the last sample")
	assert.Equal(t, StatementTimeout, artifact.SampleBudget,
		"one statement, declared: DefaultSampleBudget would charge the closing tick for two")
}

func TestTablespacesColumnOrder(t *testing.T) {
	assert.Equal(t, []string{"spcname", "pg_reported_size_bytes"}, tablespaceColumns,
		"the pair, in this order")
	assert.Equal(t, "spcname", tablespaceColumns[colSpcName], "the key leads: unique in pg_tablespace")
}

func TestTablespacesGuardTheSizeFunctionInTheSelectList(t *testing.T) {
	assert.Contains(t, tablespaceSizesSQL, "CASE WHEN",
		"an error raised in a select list aborts the statement, so the function is never "+
			"reached for a tablespace the role may not read")
	assert.Contains(t, tablespaceSizesSQL, "oid = (SELECT dattablespace FROM pg_catalog.pg_database WHERE datname = current_database())",
		"the server exempts the database's own default tablespace from the privilege check")
	assert.Contains(t, tablespaceSizesSQL, "pg_has_role(current_user, 'pg_read_all_stats', 'USAGE')",
		"USAGE, not MEMBER: the server checks has_privs_of_role, and a NOINHERIT member "+
			"would otherwise reach the function and fail the statement")
	assert.Contains(t, tablespaceSizesSQL, "has_tablespace_privilege(oid, 'CREATE')")
	assert.Contains(t, tablespaceSizesSQL, "ORDER BY spcname", "determinism, never a statistic")
	assert.NotContains(t, tablespaceSizesSQL, "LIMIT",
		"no cap: a cluster has a handful of tablespaces, not a pathological schema's worth")
}

func TestTablespacesGoldenFull(t *testing.T) {
	results := runTablespacesWindow(t, goldenClock(t), testTarget(), connectTo(newFakeTablespacesConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_tablespaces_full.txt"), artifactText(t, results[0]))
}

func TestTablespacesGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runTablespacesWindow(t, clock, testTarget(),
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_tablespaces_connect_failure.txt"), artifactText(t, results[0]))
}

func TestTablespacesGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeTablespacesConn()
	conn.sizes = queue(
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(tablespaceSizesEnd()),
	)

	results := runTablespacesWindow(t, clock, testTarget(), connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_tablespaces_sample_error.txt"), artifactText(t, results[0]))
}

func TestTablespacesGoldenLeastPrivilege(t *testing.T) {
	conn := newFakeTablespacesConn()
	conn.sizes = repeat(rowsResult(tablespaceSizesLeastPrivilege()))

	results := runTablespacesWindow(t, goldenClock(t), testTarget(), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"a role that may not read two of the three sizes still completes: the guard is "+
			"what keeps a refusal from aborting the statement")
	assert.Equal(t, bloatGolden(t, "pg_tablespaces_least_privilege.txt"), artifactText(t, results[0]))
}

func TestTablespacesUnreadSizeIsEmptyNeverZero(t *testing.T) {
	conn := newFakeTablespacesConn()
	conn.sizes = repeat(rowsResult(tablespaceSizesLeastPrivilege()))

	block := takeTablespaceSample(t, conn)

	assert.Contains(t, block, "tablespaces=3 sizes_unread=2",
		"the header counts the refusals, so a reader need not scan the rows for empty cells")

	rows := tablespaceRows(t, block)
	require.Len(t, rows, 3)

	assert.Equal(t, "", rows[0][colSpcSize], "orders_archive: CREATE not granted, so not read")
	assert.Equal(t, "3617382400", rows[1][colSpcSize],
		"pg_default: the database's own default tablespace needs no grant")
	assert.Equal(t, "", rows[2][colSpcSize], "pg_global: the shared catalogues, not read")
}

func TestTablespacesFailingStatementWritesNothing(t *testing.T) {
	conn := newFakeTablespacesConn()
	conn.sizes = repeat(errResult(errors.New("ERROR: canceling statement due to statement timeout")))

	var buf bytes.Buffer
	err := Tablespaces{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched: the window writes the stub")
}

func TestTablespacesWritesTheBlockInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Tablespaces{}.Sample(context.Background(), newFakeTablespacesConn(), writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestTablespacesNoRowsStillWritesTheColumnHeader(t *testing.T) {
	conn := newFakeTablespacesConn()
	conn.sizes = repeat(rowsResult(nil))

	block := takeTablespaceSample(t, conn)

	assert.Contains(t, block, "tablespaces=0 sizes_unread=0")
	assert.Empty(t, tablespaceRows(t, block),
		"captured and found nothing is a different shape from could not be captured, "+
			"even though a live server always has pg_default and pg_global")
}

func TestTablespacesStatementTakesNoArguments(t *testing.T) {
	conn := newFakeTablespacesConn()

	takeTablespaceSample(t, conn)

	require.Len(t, conn.args, 1)
	assert.Empty(t, conn.args[0], "no cap and no filter: the whole catalogue, every sample")
}

func TestTablespacesIdentifiersWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeTablespacesConn()
	conn.sizes = repeat(rowsResult([][]any{
		sizeRowFor("line\nbreak,\"quoted\"", counted(1)),
	}))

	block := takeTablespaceSample(t, conn)

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	rows := tablespaceRows(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "line break,\"quoted\"", rows[0][colSpcName],
		"the line break is flattened to a space; the comma and quotes survive CSV quoting")
}

// The shared reader behind pg_metadata.txt's location block and the M3 poll's
// disk reading.

type fakeTablespaceReader struct {
	*fakeWindowConn

	rows fakeResult
	sql  []string
}

func (c *fakeTablespaceReader) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = append(c.sql, sql)

	if sql != tablespaceSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	if c.rows.err != nil {
		return nil, c.rows.err
	}

	return &fakeRows{values: c.rows.rows}, nil
}

func TestReadTablespacesKeepsNameAndLocationTogether(t *testing.T) {
	conn := &fakeTablespaceReader{fakeWindowConn: newFakeWindowConn(), rows: rowsResult([][]any{
		{"orders_archive", ptr("/srv/pg/archive")},
		{"orders_fast", ptr("/mnt/nvme/pg")},
	})}

	tablespaces, err := readTablespaces(context.Background(), conn)
	require.NoError(t, err)

	assert.Equal(t, []Tablespace{
		{Name: "orders_archive", Location: "/srv/pg/archive"},
		{Name: "orders_fast", Location: "/mnt/nvme/pg"},
	}, tablespaces)

	assert.Contains(t, tablespaceSQL, "pg_tablespace_location(oid) <> ''",
		"pg_default and pg_global live in the data directory and are left to the server block's data_directory")
	assert.Contains(t, tablespaceSQL, "ORDER BY spcname")
}

func TestReadTablespacesSkipsARowWithNoLocation(t *testing.T) {
	conn := &fakeTablespaceReader{fakeWindowConn: newFakeWindowConn(), rows: rowsResult([][]any{
		{"pg_default", nil},
		{"pg_global", ptr("")},
		{"orders_archive", ptr("/srv/pg/archive")},
	})}

	tablespaces, err := readTablespaces(context.Background(), conn)
	require.NoError(t, err)

	assert.Equal(t, []Tablespace{{Name: "orders_archive", Location: "/srv/pg/archive"}}, tablespaces,
		"a location the statement should already have filtered is skipped rather than written empty")
}

func TestReadTablespacesReturnsTheStatementsError(t *testing.T) {
	conn := &fakeTablespaceReader{fakeWindowConn: newFakeWindowConn(),
		rows: errResult(errors.New("ERROR: canceling statement due to statement timeout"))}

	_, err := readTablespaces(context.Background(), conn)
	require.Error(t, err)
}
