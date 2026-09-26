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
	colSettingName = iota
	colSettingValue
	colSettingUnit
	colSettingSource
	colSettingContext
	colSettingShortDesc
)

const testReplicationPassword = "s3cr3t-Repl"

func settingRow(name, setting string, unit *string, source, settingContext, shortDesc string) []any {
	return []any{name, nullable(setting), unit, nullable(source), nullable(settingContext), nullable(shortDesc)}
}

// A standby's settings, in the statement's order. The agent's own startup
// settings are here as source=client; the only change between the samples is
// a reload that halved log_min_duration_statement.
func nonDefaultSettingsSample(logMinDuration string) [][]any {
	return [][]any{
		settingRow("application_name", ApplicationName, nil, "client", "user",
			"Sets the application name to be reported in statistics and logs."),
		settingRow("DateStyle", "ISO, MDY", nil, "configuration file", "user",
			"Sets the display format for date and time values."),
		settingRow("default_transaction_read_only", "on", nil, "client", "user",
			"Sets the default read-only status of new transactions."),
		settingRow("idle_in_transaction_session_timeout", "5000", nullable("ms"), "client", "user",
			"Sets the maximum allowed idle time between queries, when in a transaction."),
		settingRow("idle_session_timeout", "0", nullable("ms"), "client", "user",
			"Sets the maximum allowed idle time between queries, when not in a transaction."),
		settingRow("lock_timeout", "2000", nullable("ms"), "client", "user",
			"Sets the maximum allowed duration of any wait for a lock."),
		settingRow("log_min_duration_statement", logMinDuration, nullable("ms"), "configuration file", "superuser",
			"Sets the minimum execution time above which all statements will be logged."),
		settingRow("max_connections", "200", nil, "configuration file", "postmaster",
			"Sets the maximum number of concurrent connections."),
		settingRow("primary_conninfo",
			"host=10.0.4.12 port=5432 user=replicator password="+testReplicationPassword+" application_name=orders-standby",
			nil, "configuration file", "sighup",
			"Sets the connection string to be used to connect to the sending server."),
		settingRow("shared_buffers", "524288", nullable("8kB"), "configuration file", "postmaster",
			"Sets the number of shared memory buffers used by the server."),
		settingRow("statement_timeout", "10000", nullable("ms"), "client", "user",
			"Sets the maximum allowed duration of any statement."),
		settingRow("TimeZone", "Etc/UTC", nil, "configuration file", "user",
			"Sets the time zone for displaying and interpreting time stamps."),
		settingRow("work_mem", "16384", nullable("kB"), "configuration file", "user",
			"Sets the maximum memory to be used for query workspaces."),
	}
}

type fakeNonDefaultSettingsConn struct {
	*fakeWindowConn

	settings []fakeResult
	args     [][]any
}

func newFakeNonDefaultSettingsConn() *fakeNonDefaultSettingsConn {
	return &fakeNonDefaultSettingsConn{
		fakeWindowConn: newFakeWindowConn(),
		settings: queue(
			rowsResult(nonDefaultSettingsSample("1000")),
			rowsResult(nonDefaultSettingsSample("500")),
		),
	}
}

func (c *fakeNonDefaultSettingsConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != nonDefaultSettingsSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	c.args = append(c.args, args)

	return answer(&c.settings)
}

func runNonDefaultSettingsWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{NonDefaultSettings{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

// nonDefaultSettingsBlock splits one sample into its header's fields and its
// rows, the column header checked and dropped.
func nonDefaultSettingsBlock(t *testing.T, block string) (map[string]string, [][]string) {
	t.Helper()

	var (
		header string
		body   strings.Builder
	)

	for line := range strings.SplitSeq(block, "\n") {
		switch {
		case strings.HasPrefix(line, "#"):
			header = line
		case line != "":
			body.WriteString(line)
			body.WriteString("\n")
		}
	}

	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)
	require.Equal(t, nonDefaultSettingsColumns, records[0], "the column header leads every block")

	return headerFields(t, header), records[1:]
}

func takeNonDefaultSettingsSample(t *testing.T, conn *fakeNonDefaultSettingsConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, NonDefaultSettings{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	return buf.String()
}

func settingNamed(t *testing.T, rows [][]string, name string) []string {
	t.Helper()

	for _, row := range rows {
		if row[colSettingName] == name {
			return row
		}
	}

	t.Fatalf("no %s row", name)

	return nil
}

func TestNonDefaultSettingsArtifact(t *testing.T) {
	artifact := NonDefaultSettings{}.Artifact()

	assert.Equal(t, "pg_nondefault_settings", artifact.Name)
	assert.Equal(t, "pg_nondefault_settings.txt", artifact.FileName)
	assert.Equal(t, "database", artifact.Scope,
		"a database's and a role's own settings show only in sessions there")
	assert.Equal(t, Periodic(0), artifact.Schedule,
		"no cadence given is the bookend alone, never a single sample")
	assert.Equal(t, Periodic(15*time.Second), NonDefaultSettings{Interval: 15 * time.Second}.Artifact().Schedule,
		"the run's cadence, with the close as the last sample")
	assert.Equal(t, StatementTimeout, artifact.SampleBudget,
		"one statement, declared: DefaultSampleBudget would charge the closing tick for two")
}

func TestNonDefaultSettingsColumnOrder(t *testing.T) {
	assert.Equal(t, []string{"name", "setting", "unit", "source", "context", "short_desc"},
		nonDefaultSettingsColumns, "pg_settings' own names, in this order")
	assert.Equal(t, "name", nonDefaultSettingsColumns[colSettingName], "the key leads: unique in pg_settings")
}

func TestNonDefaultSettingsStatementReadsEverySettingChangedFromItsDefault(t *testing.T) {
	assert.Contains(t, nonDefaultSettingsSQL, "SELECT name, setting, unit, source, context, short_desc")
	assert.Contains(t, nonDefaultSettingsSQL, "FROM pg_catalog.pg_settings")
	assert.Contains(t, nonDefaultSettingsSQL, "WHERE source != 'default' AND source != 'override'")
	assert.Contains(t, nonDefaultSettingsSQL, "ORDER BY name")
	assert.NotContains(t, nonDefaultSettingsSQL, "LIMIT", "no cap: one row per changed setting")

	conn := newFakeNonDefaultSettingsConn()
	takeNonDefaultSettingsSample(t, conn)

	require.Len(t, conn.args, 1)
	assert.Empty(t, conn.args[0], "the statement takes no arguments")
}

func TestNonDefaultSettingsGoldenFull(t *testing.T) {
	results := runNonDefaultSettingsWindow(t, goldenClock(t), connectTo(newFakeNonDefaultSettingsConn()))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])
	assert.Equal(t, bloatGolden(t, "pg_nondefault_settings_full.txt"), artifact)
	assert.NotContains(t, artifact, testReplicationPassword)
}

func TestNonDefaultSettingsGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runNonDefaultSettingsWindow(t, clock,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_nondefault_settings_connect_failure.txt"), artifactText(t, results[0]))
}

func TestNonDefaultSettingsGoldenSampleError(t *testing.T) {
	clock := newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 15, 201),
		at(32, 15, 201),
		at(34, 5, 140),
		at(34, 5, 201),
	)

	conn := newFakeNonDefaultSettingsConn()
	conn.settings = queue(
		errResult(errors.New("ERROR: canceling statement due to statement timeout")),
		rowsResult(nonDefaultSettingsSample("500")),
	)

	results := runNonDefaultSettingsWindow(t, clock, connectTo(conn))

	require.Equal(t, StatusPartial, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_nondefault_settings_sample_error.txt"), artifactText(t, results[0]))
}

func TestNonDefaultSettingsWritesEverySettingInTheStatementsOrder(t *testing.T) {
	_, rows := nonDefaultSettingsBlock(t, takeNonDefaultSettingsSample(t, newFakeNonDefaultSettingsConn()))
	require.Len(t, rows, 13)

	assert.Equal(t, []string{"shared_buffers", "524288", "8kB", "configuration file", "postmaster",
		"Sets the number of shared memory buffers used by the server."}, rows[9])
	assert.Equal(t, []string{"application_name", ApplicationName, "", "client", "user",
		"Sets the application name to be reported in statistics and logs."}, rows[0],
		"the agent's own session settings are listed like any other, as source=client")
}

func TestNonDefaultSettingsRedactsThePasswordAndKeepsTheRow(t *testing.T) {
	block := takeNonDefaultSettingsSample(t, newFakeNonDefaultSettingsConn())
	assert.NotContains(t, block, testReplicationPassword)

	header, rows := nonDefaultSettingsBlock(t, block)
	assert.Equal(t, "1", header["redacted"], "the header counts what was replaced")

	conninfo := settingNamed(t, rows, "primary_conninfo")
	assert.Equal(t, "host=10.0.4.12 port=5432 user=replicator password=<redacted> application_name=orders-standby",
		conninfo[colSettingValue], "only the password goes: where the standby connects to stays")
	assert.Equal(t, "sighup", conninfo[colSettingContext])
}

func TestNonDefaultSettingsRedactsEverySettingAndCountsEachPassword(t *testing.T) {
	conn := newFakeNonDefaultSettingsConn()
	conn.settings = repeat(rowsResult([][]any{
		settingRow("archive_command", "pg_receivewal -d postgresql://archiver:arch-pw@backup/postgres -D /wal",
			nil, "configuration file", "sighup", "Sets the shell command that will be called to archive a WAL file."),
		settingRow("primary_conninfo", "user=replicator password='a b' sslpassword=key-pw host=primary",
			nil, "configuration file", "sighup", "Sets the connection string to be used to connect to the sending server."),
	}))

	block := takeNonDefaultSettingsSample(t, conn)
	for _, secret := range []string{"arch-pw", "'a b'", "key-pw"} {
		assert.NotContains(t, block, secret)
	}

	header, rows := nonDefaultSettingsBlock(t, block)
	assert.Equal(t, "3", header["redacted"], "one per password, across every row")
	assert.Equal(t, "pg_receivewal -d postgresql://archiver:<redacted>@backup/postgres -D /wal",
		settingNamed(t, rows, "archive_command")[colSettingValue])
	assert.Equal(t, "user=replicator password=<redacted> sslpassword=<redacted> host=primary",
		settingNamed(t, rows, "primary_conninfo")[colSettingValue])
}

func TestNonDefaultSettingsWithNoPasswordCountsZero(t *testing.T) {
	conn := newFakeNonDefaultSettingsConn()
	conn.settings = repeat(rowsResult([][]any{
		settingRow("primary_conninfo", "host=primary user=replicator passfile=/var/lib/postgresql/.pgpass",
			nil, "configuration file", "sighup", "Sets the connection string to be used to connect to the sending server."),
	}))

	header, rows := nonDefaultSettingsBlock(t, takeNonDefaultSettingsSample(t, conn))

	assert.Equal(t, "0", header["redacted"], "written when nothing was replaced, so a reader knows redaction ran")
	assert.Equal(t, "host=primary user=replicator passfile=/var/lib/postgresql/.pgpass",
		settingNamed(t, rows, "primary_conninfo")[colSettingValue], "a password file's path is not a password")
}

func TestNonDefaultSettingsNullCellsAreEmpty(t *testing.T) {
	conn := newFakeNonDefaultSettingsConn()
	conn.settings = repeat(rowsResult([][]any{
		{"max_connections", nullable("200"), nil, nullable("configuration file"), nullable("postmaster"), nil},
	}))

	_, rows := nonDefaultSettingsBlock(t, takeNonDefaultSettingsSample(t, conn))
	require.Len(t, rows, 1)

	assert.Equal(t, "", rows[0][colSettingUnit], "a setting without a unit")
	assert.Equal(t, "", rows[0][colSettingShortDesc])
}

func TestNonDefaultSettingsFailingStatementWritesNothing(t *testing.T) {
	conn := newFakeNonDefaultSettingsConn()
	conn.settings = repeat(errResult(errors.New("ERROR: canceling statement due to statement timeout")))

	var buf bytes.Buffer
	err := NonDefaultSettings{}.Sample(context.Background(), conn, &buf, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	})

	require.Error(t, err)
	assert.Empty(t, buf.String(), "a failed sample leaves the artifact untouched: the window writes the stub")
}

func TestNonDefaultSettingsWritesTheBlockInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, NonDefaultSettings{}.Sample(context.Background(), newFakeNonDefaultSettingsConn(), writer, SampleContext{
		At: at(32, 5, 112), Index: 1, Database: "orders_db", DBID: "16401",
	}))

	assert.Equal(t, 1, writer.writes,
		"a write failing between header and body would leave the window's stub behind a half-written block")
	assert.NotEmpty(t, writer.buf.String())
}

func TestNonDefaultSettingsValuesWithSeparatorsRoundTrip(t *testing.T) {
	conn := newFakeNonDefaultSettingsConn()
	conn.settings = repeat(rowsResult([][]any{
		settingRow("log_line_prefix", "%m [%p]\n\"%u\",%d ", nil, "configuration file", "sighup",
			"Controls information prefixed to each log line."),
	}))

	block := takeNonDefaultSettingsSample(t, conn)

	lines := strings.Split(strings.TrimSuffix(block, "\n"), "\n")
	require.Len(t, lines, 3, "block header, column header, and exactly one data line")

	_, rows := nonDefaultSettingsBlock(t, block)
	require.Len(t, rows, 1)

	assert.Equal(t, "%m [%p] \"%u\",%d ", rows[0][colSettingValue],
		"the line break is flattened to a space; the comma, quotes and trailing space survive CSV quoting")
}

func TestRedactPasswords(t *testing.T) {
	cases := []struct {
		name, in, out string
		redacted      int
	}{
		{"keyword form", "host=primary user=replicator password=secret application_name=standby",
			"host=primary user=replicator password=<redacted> application_name=standby", 1},
		{"a quoted value with an escaped quote", `password='a b\' c' host=primary`,
			"password=<redacted> host=primary", 1},
		{"spaces around the equals sign", "password = secret host=primary",
			"password = <redacted> host=primary", 1},
		{"an escaped space in an unquoted value", `password=a\ b host=primary`,
			"password=<redacted> host=primary", 1},
		{"after a space-only value, the next word is the value", "password= host=primary",
			"password= <redacted>", 1},
		{"an unterminated quote runs to the end", "password='never closed host=primary",
			"password=<redacted>", 1},
		{"a keyword ending in password", "sslpassword=key-pw password=secret",
			"sslpassword=<redacted> password=<redacted>", 2},
		{"any case", "PASSWORD=secret", "PASSWORD=<redacted>", 1},
		{"a shell variable", "PGPASSWORD=secret pg_receivewal -D /wal", "PGPASSWORD=<redacted> pg_receivewal -D /wal", 1},
		{"an empty value hides nothing", "password=", "password=", 0},
		{"a uri's user part", "postgresql://replicator:secret@primary:5432/postgres?sslmode=require",
			"postgresql://replicator:<redacted>@primary:5432/postgres?sslmode=require", 1},
		{"the short scheme", "postgres://replicator:secret@primary/postgres",
			"postgres://replicator:<redacted>@primary/postgres", 1},
		{"a raw @ in a uri's password", "postgresql://u:p@ss@primary/postgres",
			"postgresql://u:<redacted>@primary/postgres", 1},
		{"a uri with several hosts", "postgresql://u:secret@h1:5432,h2:5433/postgres",
			"postgresql://u:<redacted>@h1:5432,h2:5433/postgres", 1},
		{"a uri's password parameters", "postgresql://primary/postgres?password=secret&sslpassword=key-pw&sslmode=require",
			"postgresql://primary/postgres?password=<redacted>&sslpassword=<redacted>&sslmode=require", 2},
		{"a percent-encoded parameter name", "postgresql://primary/postgres?pass%77ord=secret",
			"postgresql://primary/postgres?pass%77ord=<redacted>", 1},
		{"both places at once", "postgresql://u:one@primary/postgres?password=two",
			"postgresql://u:<redacted>@primary/postgres?password=<redacted>", 2},
		{"a uri inside a command", "pg_basebackup -d postgres://u:secret@primary/postgres -D /data",
			"pg_basebackup -d postgres://u:<redacted>@primary/postgres -D /data", 1},
		{"a uri without a password", "postgresql://replicator@primary:5432/postgres",
			"postgresql://replicator@primary:5432/postgres", 0},
		{"a port is not a password", "postgresql://primary:5432/postgres?application_name=a",
			"postgresql://primary:5432/postgres?application_name=a", 0},
		{"an empty uri password hides nothing", "postgresql://u:@primary/postgres",
			"postgresql://u:@primary/postgres", 0},
		{"a uri as a keyword's value is hidden whole", "password=postgresql://u:p@h",
			"password=<redacted>", 1},
		{"a value with no password", "scram-sha-256", "scram-sha-256", 0},
		{"a password file's path", "passfile=/var/lib/postgresql/.pgpass", "passfile=/var/lib/postgresql/.pgpass", 0},
		{"empty", "", "", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, redacted := redactPasswords(c.in)

			assert.Equal(t, c.out, out)
			assert.Equal(t, c.redacted, redacted)
		})
	}
}
