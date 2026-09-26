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
	colXIDAgeDatName = iota
	colXIDAgeAge
)

func xidAgeRowFor(name string, age int64) []any {
	return []any{name, counted(age)}
}

// Oldest first, which is the statement's order. The transaction-ID counter is
// the cluster's, so every database ages by the same 90492 between the samples;
// only a vacuum that freezes a database moves it back.
func xidAgesStart() [][]any {
	return [][]any{
		xidAgeRowFor("orders_db", 412883019),
		xidAgeRowFor("postgres", 201554812),
		xidAgeRowFor("template1", 201554810),
		xidAgeRowFor("template0", 198204233),
	}
}

func xidAgesEnd() [][]any {
	return [][]any{
		xidAgeRowFor("orders_db", 412973511),
		xidAgeRowFor("postgres", 201645304),
		xidAgeRowFor("template1", 201645302),
		xidAgeRowFor("template0", 198294725),
	}
}

type fakeXIDAgeConn struct {
	*fakeWindowConn

	ages []fakeResult
	args [][]any
}

func newFakeXIDAgeConn() *fakeXIDAgeConn {
	return &fakeXIDAgeConn{
		fakeWindowConn: newFakeWindowConn(),
		ages:           queue(rowsResult(xidAgesStart()), rowsResult(xidAgesEnd())),
	}
}

func (c *fakeXIDAgeConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != xidAgeSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	c.args = append(c.args, args)

	return answer(&c.ages)
}

func runXIDAgeWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{XIDAge{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func xidAgeRows(t *testing.T, block string) [][]string {
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
	require.Equal(t, xidAgeColumns, records[0], "the column header leads every block")

	return records[1:]
}

func takeXIDAgeSample(t *testing.T, conn *fakeXIDAgeConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, XIDAge{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func TestXIDAgeArtifact(t *testing.T) {
	artifact := XIDAge{}.Artifact()

	assert.Equal(t, "pg_xid_age", artifact.Name)
	assert.Equal(t, "pg_xid_age.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"every database in the cluster: db= and dbid= mean connected through, not about")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), XIDAge{Interval: 15 * time.Second}.Artifact().Schedule,
		"the run's cadence, with the close as the last sample")
	assert.Equal(t, StatementTimeout, artifact.SampleBudget,
		"one statement, declared: DefaultSampleBudget would charge the closing tick for two")
}

func TestXIDAgeColumnOrder(t *testing.T) {
	assert.Equal(t, []string{"datname", "xid_age"}, xidAgeColumns, "the pair, in this order")
	assert.Equal(t, "datname", xidAgeColumns[colXIDAgeDatName], "the key leads: unique in pg_database")
}

func TestXIDAgeStatementReadsEveryDatabaseOldestFirst(t *testing.T) {
	assert.Contains(t, xidAgeSQL, "age(datfrozenxid) AS xid_age")
	assert.Contains(t, xidAgeSQL, "FROM pg_catalog.pg_database")
	assert.Contains(t, xidAgeSQL, "ORDER BY xid_age DESC")
	assert.NotContains(t, xidAgeSQL, "LIMIT", "no cap: one row per database")
	assert.NotContains(t, xidAgeSQL, "WHERE", "and no filter: template0 wraps around too")

	conn := newFakeXIDAgeConn()
	takeXIDAgeSample(t, conn)

	require.Len(t, conn.args, 1)
	assert.Empty(t, conn.args[0], "the statement takes no arguments")
}

func TestXIDAgeGoldenFull(t *testing.T) {
	results := runXIDAgeWindow(t, goldenClock(t), connectTo(newFakeXIDAgeConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_xid_age_full.txt"), artifactText(t, results[0]))
}

func TestXIDAgeGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runXIDAgeWindow(t, clock,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_xid_age_connect_failure.txt"), artifactText(t, results[0]))
}

func TestXIDAgeGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeXIDAgeConn()
	conn.ages = queue(
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(xidAgesEnd()),
	)

	results := runXIDAgeWindow(t, clock, connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_xid_age_sample_error.txt"), artifactText(t, results[0]))
}

func TestXIDAgeWritesEveryDatabaseInTheStatementsOrder(t *testing.T) {
	rows := xidAgeRows(t, takeXIDAgeSample(t, newFakeXIDAgeConn()))
	require.Len(t, rows, 4)

	assert.Equal(t, []string{"orders_db", "412883019"}, rows[0], "oldest first")
	assert.Equal(t, []string{"template0", "198204233"}, rows[3],
		"template0 is listed like any other: it cannot be connected to, but it still ages")
}

func TestXIDAgeFailingStatementWritesNothing(t *testing.T) {
	conn := newFakeXIDAgeConn()
	conn.ages = repeat(errResult(errors.New("ERROR: canceling statement due to statement timeout")))

	var buf bytes.Buffer
	err := XIDAge{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched: the window writes the stub")
}

func TestXIDAgeWritesTheBlockInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, XIDAge{}.Sample(context.Background(), newFakeXIDAgeConn(), writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestXIDAgeIdentifiersWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeXIDAgeConn()
	conn.ages = repeat(rowsResult([][]any{xidAgeRowFor("line\nbreak,\"quoted\"", 1)}))

	block := takeXIDAgeSample(t, conn)

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	rows := xidAgeRows(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "line break,\"quoted\"", rows[0][colXIDAgeDatName],
		"the line break is flattened to a space; the comma and quotes survive CSV quoting")
}
