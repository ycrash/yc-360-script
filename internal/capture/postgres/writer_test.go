package postgres

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Metadata for the goldens is built by hand rather than run through Collect, so
// a golden that changes points at the writer and nothing else.

var testConnectFailureNow = time.Date(2026, 8, 4, 9, 12, 49, 201_000_000, time.UTC)

// fullArtifactMetadata is testdata/pg_metadata_full.txt. PostgreSQL 17
// deliberately: EXECUTE on pg_current_logfile() was not granted to pg_monitor
// until 17, so on 14-16 has_pg_monitor_role,true next to capture_mode,pg-dbhost
// would depict a deployment nobody can reproduce.
func fullArtifactMetadata() Metadata {
	return Metadata{
		AgentTS:        testAgentNow,
		YC360Version:   "3.6.1",
		TargetHost:     "db-prod-01.internal",
		TargetPort:     5432,
		TargetDatabase: "orders_db",
		TargetUsername: "ycrash_monitor",
		TargetSSLMode:  "require",

		CaptureMode: ModeDBHost,

		CurrentDatabase:     "orders_db",
		CurrentUser:         "ycrash_monitor",
		BackendPID:          "48211",
		InetServerAddr:      "10.0.4.7",
		InetServerPort:      "5432",
		IsInRecovery:        "false",
		PostmasterStartTime: timestamp(testPostmasterStart),
		StatsReset:          timestamp(testStatsReset),
		Version:             "PostgreSQL 17.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0",
		ServerVersionNum:    "170004",

		MaxConnections:          "200",
		LoggingCollector:        "on",
		LogDestination:          "csvlog",
		LogDirectory:            "log",
		LogFilename:             "postgresql-%Y-%m-%d_%H%M%S.log",
		LogLinePrefix:           "%m [%p] ",
		LogMinDurationStatement: "500",
		LogParameterMaxLength:   "1024",
		SharedPreloadLibraries:  "pg_stat_statements,auto_explain",

		DataDirectory:          "/var/lib/postgresql/17/main",
		CurrentLogfile:         "log/postgresql-2026-08-04_000000.csv",
		CurrentLogfileResolved: "/var/lib/postgresql/17/main/log/postgresql-2026-08-04_000000.csv",
		CurrentLogfileReadable: "true",

		HasPgMonitorRole:        "true",
		HasPgStatStatements:     "true",
		PgStatStatementsVersion: "1.11",
		HasPgStatCheckpointer:   "true",
		HasSessionFatalStats:    "true",
		ComputeQueryID:          "auto",

		ReplicationConfigured: "true",

		ServerNow:            timestamp(testServerNow),
		ServerClockTimestamp: timestamp(testServerClock),
		AgentTSAtClockRead:   testAgentNow,
	}
}

// connectFailureMetadata is testdata/pg_metadata_connect_failure.txt.
func connectFailureMetadata() Metadata {
	return Metadata{
		AgentTS:            testConnectFailureNow,
		AgentTSAtClockRead: testConnectFailureNow,
		YC360Version:       "3.6.1",
		TargetHost:         "db-prod-01.internal",
		TargetPort:         5432,
		TargetDatabase:     "orders_db",
		TargetUsername:     "ycrash_monitor",
		TargetSSLMode:      "require",

		CaptureMode:  ModeUnknown,
		ConnectError: ErrTooManyConnections.Error(),
	}
}

// writeArtifact composes the two passes the way the adapter does.
func writeArtifact(t *testing.T, m Metadata) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, WriteTarget(&buf, m))
	require.NoError(t, WriteResult(&buf, m))

	return buf.String()
}

func golden(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return string(content)
}

// parseArtifact reads an artifact the way the block contract says a reader
// should: CSV records, not lines.
func parseArtifact(t *testing.T, artifact string) (header string, values map[string]string, keys []string) {
	t.Helper()

	reader := csv.NewReader(strings.NewReader(artifact))
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	require.NoError(t, err)
	require.Greater(t, len(records), 1)

	require.Len(t, records[0], 1, "the block header is one field, so it carries no comma")
	require.True(t, strings.HasPrefix(records[0][0], "#"))
	require.Equal(t, []string{"key", "value"}, records[1])

	values = map[string]string{}
	for _, record := range records[2:] {
		require.Len(t, record, 2)

		values[record[0]] = record[1]
		keys = append(keys, record[0])
	}

	return records[0][0], values, keys
}

func TestWriteFullArtifact(t *testing.T) {
	assert.Equal(t, golden(t, "pg_metadata_full.txt"), writeArtifact(t, fullArtifactMetadata()))
}

func TestWriteConnectFailure(t *testing.T) {
	assert.Equal(t, golden(t, "pg_metadata_connect_failure.txt"), writeArtifact(t, connectFailureMetadata()))
}

// Guards a byte an editor cannot see: log_line_prefix's default ends in a
// space, and a trimming save would silently change the golden.
func TestGoldenKeepsTrailingWhitespace(t *testing.T) {
	assert.Contains(t, golden(t, "pg_metadata_full.txt"), "log_line_prefix,%m [%p] \n",
		"the golden's log_line_prefix must keep its trailing space")
}

func TestBlockHeaderFieldOrder(t *testing.T) {
	header, _, _ := parseArtifact(t, writeArtifact(t, fullArtifactMetadata()))

	assert.Equal(t, []string{
		"#",
		"engine=postgres",
		"source=pg_metadata",
		"v=1",
		"format=csv",
		"scope=cluster",
		"ts=2026-08-04T09:12:44.118Z",
	}, strings.Fields(header))
}

func TestConnectErrorIsTheDiscriminator(t *testing.T) {
	m := fullArtifactMetadata()
	m.CaptureMode = ModeUnknown
	m.ConnectError = ErrTooManyConnections.Error()

	_, values, keys := parseArtifact(t, writeArtifact(t, m))

	assert.Equal(t, "connect_error", keys[len(keys)-1])
	assert.NotContains(t, values, "current_database")
}

func TestQueryErrorStillWritesEveryKey(t *testing.T) {
	m := connectFailureMetadata()
	m.AgentTS = testAgentNow
	m.AgentTSAtClockRead = testAgentNow
	m.CaptureMode = ModeRemote
	m.ConnectError = ""
	m.QueryError = "ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"

	_, values, keys := parseArtifact(t, writeArtifact(t, m))

	_, _, want := parseArtifact(t, golden(t, "pg_metadata_full.txt"))
	assert.Equal(t, want, keys, "the key set does not depend on what could be read")

	assert.Equal(t, m.QueryError, values["query_error"])
	for _, key := range []string{
		"current_database",
		"server_version_num",
		"max_connections",
		"has_pg_stat_checkpointer",
		"server_now",
	} {
		assert.Empty(t, values[key], "%s should be written empty, not filled", key)
	}
}

// The values that made encoding/csv the right writer.
func TestAwkwardValuesRoundTrip(t *testing.T) {
	m := fullArtifactMetadata()
	m.LogLinePrefix = `%m [%p] app=%a,user=%u "session"`
	m.Version = "PostgreSQL 17.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0, 64-bit"
	m.SharedPreloadLibraries = "pg_stat_statements,auto_explain,pg_cron"

	_, values, _ := parseArtifact(t, writeArtifact(t, m))

	assert.Equal(t, m.LogLinePrefix, values["log_line_prefix"])
	assert.Equal(t, m.Version, values["version"])
	assert.Equal(t, m.SharedPreloadLibraries, values["shared_preload_libraries"])
}

// encoding/csv would quote both of these correctly; flattening is what also
// makes one row one line, for readers not using a CSV parser.
func TestValuesAreFlattened(t *testing.T) {
	m := fullArtifactMetadata()
	m.LogLinePrefix = "%m [%p]\n> "
	m.QueryError = "ERROR: permission denied\r\nDETAIL: role is not a member"

	artifact := writeArtifact(t, m)
	_, values, keys := parseArtifact(t, artifact)

	assert.Equal(t, "%m [%p] > ", values["log_line_prefix"])
	assert.Equal(t, "ERROR: permission denied DETAIL: role is not a member", values["query_error"])

	lines := strings.Split(strings.TrimSuffix(artifact, "\n"), "\n")
	assert.Len(t, lines, len(keys)+2, "one row is one line, plus the block and column headers")
}

// The property the two-pass split exists for: a process killed between the
// passes still leaves a parseable file saying what was targeted.
func TestWriteTargetAlone(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteTarget(&buf, fullArtifactMetadata()))

	header, values, keys := parseArtifact(t, buf.String())

	assert.Contains(t, header, "engine=postgres")
	assert.Equal(t, []string{
		"agent_ts",
		"yc360_version",
		"target_host",
		"target_port",
		"target_database",
		"target_username",
		"target_sslmode",
	}, keys)
	assert.Equal(t, "db-prod-01.internal", values["target_host"])
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// encoding/csv defers write errors to Flush, so the oversized value exercises
// the other path - a row larger than the internal buffer makes cw.Write itself
// report the failure mid-loop.
func TestWriteErrorsPropagate(t *testing.T) {
	sinkErr := errors.New("no space left on device")
	sink := failingWriter{err: sinkErr}

	m := fullArtifactMetadata()
	assert.ErrorIs(t, WriteTarget(sink, m), sinkErr)
	assert.ErrorIs(t, WriteResult(sink, m), sinkErr)

	m.QueryError = strings.Repeat("x", 1<<14)
	assert.ErrorIs(t, WriteResult(sink, m), sinkErr)
}
