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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colDatid = iota
	colDatname
	colBlksHit
	colBlksRead
	colXactCommit
	colXactRollback
	colTempFiles
	colTempBytes
	colDeadlocks
	colSessionsFatal
	colDBStatsReset
)

var testDBStatsReset = time.Date(2026, 7, 20, 2, 0, 0, 0, time.UTC)

type dbStat struct {
	datid     uint32
	datname   *string
	hit       int64
	read      int64
	commit    int64
	rollback  int64
	files     int64
	bytes     int64
	deadlocks int64
	fatal     int64
	reset     *time.Time
}

type cluster []dbStat

func (c cluster) rows() [][]any { return c.rowsOf(int64(len(c)), true) }

func (c cluster) rowsTotal(total int64) [][]any { return c.rowsOf(total, true) }

func (c cluster) withoutSessionsFatal() [][]any { return c.rowsOf(int64(len(c)), false) }

func (c cluster) rowsOf(total int64, sessionsFatal bool) [][]any {
	rows := make([][]any, len(c))

	for i, stat := range c {
		fatal := ptr(stat.fatal)
		if !sessionsFatal {
			fatal = nil
		}

		rows[i] = []any{
			stat.datid, stat.datname,
			ptr(stat.hit), ptr(stat.read), ptr(stat.commit), ptr(stat.rollback),
			ptr(stat.files), ptr(stat.bytes), ptr(stat.deadlocks), fatal,
			stat.reset, total,
		}
	}

	return rows
}

func ordersCluster(orders dbStat) cluster {
	return cluster{
		{datid: 0, hit: 182044, read: 9120},
		{datid: 1, datname: ptr("template1"), hit: 88210, read: 4021, commit: 1204, reset: &testDBStatsReset},
		{datid: 4, datname: ptr("template0")},
		{datid: 5, datname: ptr("postgres"), hit: 412008, read: 18804, commit: 8821, rollback: 12,
			reset: &testDBStatsReset},
		orders,
	}
}

func ordersHealthSample1() cluster {
	return ordersCluster(dbStat{
		datid: 16401, datname: ptr("orders_db"),
		hit: 8823401, read: 158220, commit: 442198, rollback: 9532,
		files: 4, bytes: 268435456, deadlocks: 12, fatal: 3, reset: &testDBStatsReset,
	})
}

func ordersHealthSample2() cluster {
	sample := ordersCluster(dbStat{
		datid: 16401, datname: ptr("orders_db"),
		hit: 8841820, read: 158910, commit: 442340, rollback: 9538,
		files: 5, bytes: 356515840, deadlocks: 13, fatal: 4, reset: &testDBStatsReset,
	})

	sample[0].hit = 182101
	sample[3].hit = 412160
	sample[3].commit = 8823

	return sample
}

func ordersHealthSample3() cluster {
	sample := ordersCluster(dbStat{
		datid: 16401, datname: ptr("orders_db"),
		hit: 8859944, read: 159602, commit: 442481, rollback: 9544,
		files: 5, bytes: 356515840, deadlocks: 15, fatal: 4, reset: &testDBStatsReset,
	})

	sample[0].hit = 182166
	sample[0].read = 9121
	sample[3].hit = 412311
	sample[3].commit = 8825

	return sample
}

type fakeHealthConn struct {
	*fakeWindowConn

	stats    []fakeResult
	fallback []fakeResult

	statsArgs    [][]any
	fallbackArgs [][]any
}

func newFakeHealthConn() *fakeHealthConn {
	return &fakeHealthConn{
		fakeWindowConn: newFakeWindowConn(),
		stats: queue(
			rowsResult(ordersHealthSample1().rows()),
			rowsResult(ordersHealthSample2().rows()),
			rowsResult(ordersHealthSample3().rows()),
		),
	}
}

func (c *fakeHealthConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	switch sql {
	case healthSQL:
		c.statsArgs = append(c.statsArgs, args)
		return answer(&c.stats)

	case healthSQLNoSessionsFatal:
		c.fallbackArgs = append(c.fallbackArgs, args)
		return answer(&c.fallback)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func undefinedColumn42703() error {
	return &pgconn.PgError{
		Severity: "ERROR",
		Code:     undefinedColumn,
		Message:  `column "sessions_fatal" does not exist`,
	}
}

func healthGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 61),
		at(32, 5, 61),
		at(32, 15, 58),
		at(32, 15, 58),
		at(32, 25, 55),
		at(32, 35, 2),
	)
}

func runHealthWindow(t *testing.T, clock *scriptedClock, target Target,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     target,
		Duration:   30 * time.Second,
		Collectors: []Collector{Health{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func takeHealthSample(t *testing.T, conn *fakeHealthConn, collector Health) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 61), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func healthSampleRows(t *testing.T, block string) [][]string {
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
	require.Equal(t, healthColumns, records[0], "the column header leads every block")

	return records[1:]
}

func TestHealthArtifact(t *testing.T) {
	artifact := Health{}.Artifact()

	assert.Equal(t, "pg_health", artifact.Name)
	assert.Equal(t, "pg_health.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"pg_stat_database is read unfiltered, so the rows are about the cluster")
	assert.Equal(t, Every(DefaultHealthInterval), artifact.Schedule)

	assert.Len(t, artifact.Schedule.offsets(120*time.Second), 12,
		"twelve samples at the default window, asserted where someone changing the "+
			"constant will see it")

	assert.Equal(t, Every(time.Second), Health{Interval: time.Second}.Artifact().Schedule,
		"a test can lower the cadence without waiting out a window")
}

func TestHealthColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"datid",
		"datname",
		"blks_hit",
		"blks_read",
		"xact_commit",
		"xact_rollback",
		"temp_files",
		"temp_bytes",
		"deadlocks",
		"sessions_fatal",
		"stats_reset",
	}, healthColumns, "the server's merge contract, not a presentation choice")

	assert.Equal(t, "datid", healthColumns[colDatid], "the join key leads")
	assert.Equal(t, "stats_reset", healthColumns[colDBStatsReset],
		"and the one column that can invalidate a delta closes")
}

func TestHealthGoldenFull(t *testing.T) {
	results := runHealthWindow(t, healthGoldenClock(t), testTarget(),
		connectTo(newFakeHealthConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 3, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_health_full.txt"), artifactText(t, results[0]))
}

func TestHealthGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runHealthWindow(t, clock, testTarget(),
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_health_connect_failure.txt"), artifactText(t, results[0]))
}

func TestHealthGoldenSampleError(t *testing.T) {
	conn := newFakeHealthConn()
	conn.stats = queue(
		rowsResult(ordersHealthSample1().rows()),
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(ordersHealthSample3().rows()),
	)

	results := runHealthWindow(t, healthGoldenClock(t), testTarget(), connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 2, results[0].SamplesWritten, "the window does not stop: sample 3 is still taken")
	assert.Equal(t, bloatGolden(t, "pg_health_sample_error.txt"), artifactText(t, results[0]))
}

func TestHealthGoldenNoSessionsFatal(t *testing.T) {
	conn := newFakeHealthConn()
	conn.stats = repeat(errResult(undefinedColumn42703()))
	conn.fallback = queue(
		rowsResult(ordersHealthSample1().withoutSessionsFatal()),
		rowsResult(ordersHealthSample2().withoutSessionsFatal()),
		rowsResult(ordersHealthSample3().withoutSessionsFatal()),
	)

	results := runHealthWindow(t, healthGoldenClock(t), testTarget(), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"ten of eleven columns is a complete sample, not a failed one")
	assert.Equal(t, bloatGolden(t, "pg_health_no_sessions_fatal.txt"), artifactText(t, results[0]))
}

func TestHealthWritesNullsEmptyNeverZero(t *testing.T) {
	rows := healthSampleRows(t, takeHealthSample(t, newFakeHealthConn(), Health{}))
	require.Len(t, rows, 5)

	assert.Equal(t, "0", rows[0][colDatid])
	assert.Empty(t, rows[0][colDatname],
		"the shared-objects row accounts for shared relations, not a database: NULL, not a failed read")
	assert.Empty(t, rows[0][colDBStatsReset], "and it has never been reset")
	assert.Equal(t, "0", rows[0][colDeadlocks],
		"a zero counter is a reading and must survive as 0 next to an empty datname")

	assert.Equal(t, "template0", rows[2][colDatname])
	assert.Empty(t, rows[2][colDBStatsReset], "a template that has never been reset is empty, not an epoch")
	assert.Equal(t, "2026-07-20T02:00:00.000Z", rows[3][colDBStatsReset])

	assert.Equal(t, []string{
		"16401", "orders_db", "8823401", "158220", "442198", "9532",
		"4", "268435456", "12", "3", "2026-07-20T02:00:00.000Z",
	}, rows[4], "and every column of the connected database round-trips in order")
}

func TestHealthUnfilteredKeepsDatabasesTheAgentIsNotConnectedTo(t *testing.T) {
	block := takeHealthSample(t, newFakeHealthConn(), Health{})

	assert.Contains(t, block, "scope=cluster db=orders_db dbid=16401",
		"db= and dbid= mean connected through, not about")
	assert.Contains(t, block, "databases_written=5 databases_total=5 truncated=false")

	rows := healthSampleRows(t, block)
	require.Len(t, rows, 5)

	var datids []string
	for _, row := range rows {
		datids = append(datids, row[colDatid])
	}

	assert.Equal(t, []string{"0", "1", "4", "5", "16401"}, datids,
		"every database, including the two templates, in ascending datid")
}

func TestHealthRetriesWithoutSessionsFatalOnlyOn42703(t *testing.T) {
	t.Run("42703 costs one column, not the sample", func(t *testing.T) {
		conn := newFakeHealthConn()
		conn.stats = repeat(errResult(undefinedColumn42703()))
		conn.fallback = repeat(rowsResult(ordersHealthSample1().withoutSessionsFatal()))

		block := takeHealthSample(t, conn, Health{})

		assert.Contains(t, block,
			`sessions_fatal=unavailable reason="ERROR: column \"sessions_fatal\" does not exist (SQLSTATE 42703)"`,
			"the reason is quoted, so driver text cannot break k=v tokenisation")
		assert.Contains(t, block, "databases_written=5 databases_total=5 truncated=false",
			"the sample is still written and still counted")

		rows := healthSampleRows(t, block)
		require.Len(t, rows, 5)

		for _, row := range rows {
			assert.Empty(t, row[colSessionsFatal],
				"the column stays in the header and every cell is empty")
			assert.NotEmpty(t, row[colBlksHit], "every other column survives")
		}

		require.Len(t, conn.fallbackArgs, 1, "exactly one retry")
	})

	t.Run("any other error is an ordinary sample failure", func(t *testing.T) {
		conn := newFakeHealthConn()
		conn.stats = repeat(errResult(
			errors.New("ERROR: canceling statement due to statement timeout")))

		var buf bytes.Buffer
		err := Health{}.Sample(context.Background(), conn, &buf, SampleContext{
			At: at(32, 5, 61), Index: 1, Database: "orders_db", DBID: "16401",
		})

		require.Error(t, err)
		assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched")
		assert.Empty(t, conn.fallbackArgs, "and does not retry: the fallback is for one SQLSTATE only")
	})

	t.Run("a failed retry is a failed sample", func(t *testing.T) {
		conn := newFakeHealthConn()
		conn.stats = repeat(errResult(undefinedColumn42703()))
		conn.fallback = repeat(errResult(errors.New("ERROR: permission denied")))

		var buf bytes.Buffer
		err := Health{}.Sample(context.Background(), conn, &buf, SampleContext{
			At: at(32, 5, 61), Index: 1, Database: "orders_db", DBID: "16401",
		})

		require.Error(t, err)
		assert.Empty(t, buf.String())
	})
}

func TestHealthFallbackReasonIsRedacted(t *testing.T) {
	conn := newFakeHealthConn()
	conn.stats = repeat(errResult(fmt.Errorf("%w: dsn was postgres://u:%s@h/db",
		undefinedColumn42703(), testPassword)))
	conn.fallback = repeat(rowsResult(ordersHealthSample1().withoutSessionsFatal()))

	results := runHealthWindow(t, healthGoldenClock(t), testTarget(), connectTo(conn))

	artifact := artifactText(t, results[0])
	assert.NotContains(t, artifact, testPassword)
	assert.Contains(t, artifact, "<redacted>")
}

func TestHealthCapKeepsTheSharedRowAndTheConnectedDatabase(t *testing.T) {
	conn := newFakeHealthConn()
	conn.stats = repeat(rowsResult(cluster{
		{datid: 0, hit: 182044, read: 9120},
		{datid: 1, datname: ptr("template1"), hit: 88210},
		{datid: 16401, datname: ptr("orders_db"), hit: 8823401, read: 158220, fatal: 3,
			reset: &testDBStatsReset},
	}.rowsTotal(4120)))

	block := takeHealthSample(t, conn, Health{MaxDatabases: 3})

	assert.Contains(t, block, "databases_written=3 databases_total=4120 truncated=true",
		"a capped block must not read as a complete one")

	rows := healthSampleRows(t, block)
	require.Len(t, rows, 3)

	assert.Equal(t, []string{"0", "1", "16401"}, []string{
		rows[0][colDatid], rows[1][colDatid], rows[2][colDatid],
	}, "a capped block still reads in ascending datid, and template0 is the row that went")

	assert.Equal(t, "orders_db", rows[2][colDatname],
		"the connected database survives the cap whatever its OID")

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, []any{3, ptr(uint32(16401))}, conn.statsArgs[0],
		"the cap is the LIMIT the server is sent, and $2 is the OID the inner ordering protects")
}

func TestHealthDefaultCapIsSentWhenUnset(t *testing.T) {
	conn := newFakeHealthConn()

	takeHealthSample(t, conn, Health{})

	require.Len(t, conn.statsArgs, 1)
	assert.Equal(t, DefaultMaxDatabases, conn.statsArgs[0][0])
}

func TestHealthUnidentifiedDatabaseSendsNoOID(t *testing.T) {
	conn := newFakeHealthConn()

	var buf bytes.Buffer
	require.NoError(t, Health{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 61), Index: 1, Database: "orders_configured",
	}))

	require.Len(t, conn.statsArgs, 1)
	assert.Nil(t, conn.statsArgs[0][1],
		"identify failed, so there is no OID to protect and COALESCE protects the shared row twice")

	assert.Nil(t, connectedOID(""))
	assert.Nil(t, connectedOID("not an oid"))
	assert.Equal(t, ptr(uint32(16401)), connectedOID("16401"))
}

func TestHealthWritesTheBlockInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Health{}.Sample(context.Background(), newFakeHealthConn(), writer,
		SampleContext{At: at(32, 5, 61), Index: 1, Database: "orders_db", DBID: "16401"}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestHealthDatabaseNamesWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeHealthConn()
	conn.stats = repeat(rowsResult(cluster{
		{datid: 16401, datname: ptr("we,ird\"db\nname"), hit: 1, read: 2, fatal: 3},
	}.rows()))

	block := takeHealthSample(t, conn, Health{})

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	rows := healthSampleRows(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "we,ird\"db name", rows[0][colDatname],
		"a database name is user-chosen and a quoted identifier may legally contain a line break")
}
