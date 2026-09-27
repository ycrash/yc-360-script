package capture

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

const pgTestPassword = "s3cr3t-do-not-log"

func unreachablePostgres(t *testing.T) *config.Postgres {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	return &config.Postgres{
		Host:     "127.0.0.1",
		Port:     port,
		Database: "orders_db",
		Username: "ycrash_monitor",
		Password: pgTestPassword,
	}
}

func chdirToCaptureDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	previous, err := os.Getwd()
	require.NoError(t, err)

	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(previous) })

	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	return resolved
}

func readPostgresArtifact(t *testing.T) (raw string, values map[string]string) {
	t.Helper()

	content, err := os.ReadFile(PostgresMetadataFileName)
	require.NoError(t, err)

	var body strings.Builder
	for line := range strings.SplitSeq(string(content), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}

	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)

	values = map[string]string{}
	for _, record := range records {
		require.Len(t, record, 2)

		// The two column headers: the key,value blocks' and the tablespace block's.
		if record[0] == "key" || record[0] == "spcname" {
			continue
		}

		values[record[0]] = record[1]
	}

	return string(content), values
}

func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()

	previous := logger.GetLogger()
	t.Cleanup(func() { logger.SetLogger(previous) })

	var buf bytes.Buffer
	redirected := zerolog.New(&buf)
	logger.SetLogger(&redirected)

	return &buf
}

type recordedPostgresUpload struct {
	dt   string
	body string
}

func TestPostgresDataTypeConstant(t *testing.T) {
	assert.Equal(t, "pgMeta", pgDTMetadata)

	assert.Equal(t, "pgBloat", pgDTBloat)

	assert.Equal(t, "pgHealth", pgDTHealth)

	assert.Equal(t, "pgCapacity", pgDTCapacity)

	assert.Equal(t, "pgReplication", pgDTReplication,
		"the unabbreviated form the server team assigned, not the pgRepl this slice proposed")

	assert.Equal(t, "pgSlowQueries", pgDTSlowQueries,
		"assigned as proposed, from the namespace the previous slices established")

	assert.Equal(t, "pgSessions", pgDTSessions,
		"assigned as proposed - and the artifact the product is built on, so the value it "+
			"waited on is the one worth naming first")

	assert.Equal(t, "pgDeadlocks", pgDTDeadlocks)

	assert.Equal(t, "pgTimeouts", pgDTTimeouts)

	taken := []string{
		"meta", "gc", "td", "hd", "ns", "df", "ps", "top", "vmstat", "dmesg",
		"agentlog", "cpuprofile", "kernel", "ping", "hdsub", "lp", "accessLog", "applog",
		nodeDTProcessOverview, nodeDTEventLoopLag, nodeDTUnhandledRejections,
		nodeDTModuleInventory, nodeDTHandleGrowth, nodeDTGCStats,
	}

	postgresDataTypes := map[string]string{
		"pgDTMetadata":           pgDTMetadata,
		"pgDTBloat":              pgDTBloat,
		"pgDTHealth":             pgDTHealth,
		"pgDTCapacity":           pgDTCapacity,
		"pgDTReplication":        pgDTReplication,
		"pgDTSessions":           pgDTSessions,
		"pgDTSlowQueries":        pgDTSlowQueries,
		"pgDTDeadlocks":          pgDTDeadlocks,
		"pgDTTimeouts":           pgDTTimeouts,
		"pgDTExplain":            pgDTExplain,
		"pgDTIndexUsage":         pgDTIndexUsage,
		"pgDTTablespaces":        pgDTTablespaces,
		"pgDTCheckpointLog":      pgDTCheckpointLog,
		"pgDTXIDAge":             pgDTXIDAge,
		"pgDTNonDefaultSettings": pgDTNonDefaultSettings,
		"pgDTCatalogMap":         pgDTCatalogMap,
		"pgDTMemory":             pgDTMemory,
		"pgDTErrors":             pgDTErrors,
	}

	assert.Len(t, postgresDataTypes, len(postgresArtifactFiles)-len(postgresArtifactsAwaitingDT),
		"one dt per artifact the run writes, less any still waiting on the server team: "+
			"those are written into the bundle, held back from upload, and the run says so")

	for name, dt := range postgresDataTypes {
		assert.NotContains(t, taken, dt, "%s collides with an existing dt value", name)
	}

	seen := map[string]string{}
	for name, dt := range postgresDataTypes {
		assert.NotContains(t, seen, dt, "%s and %s share a dt", name, seen[dt])
		seen[dt] = name
	}
}

func TestPostgresBundleFileNames(t *testing.T) {
	assert.Equal(t, "pg_metadata.txt", PostgresMetadataFileName)
	assert.Equal(t, "pg_health.txt", PostgresHealthFileName)
	assert.Equal(t, "pg_bloat.txt", PostgresBloatFileName)
	assert.Equal(t, "pg_capacity.txt", PostgresCapacityFileName)
	assert.Equal(t, "pg_replication.txt", PostgresReplicationFileName)
	assert.Equal(t, "pg_sessions.txt", PostgresSessionsFileName)
	assert.Equal(t, "pg_slow_queries.txt", PostgresSlowQueriesFileName)
	assert.Equal(t, "pg_deadlocks.txt", PostgresDeadlocksFileName)
	assert.Equal(t, "pg_timeouts.txt", PostgresTimeoutsFileName)
	assert.Equal(t, "pg_explain.txt", PostgresExplainFileName)
	assert.Equal(t, "pg_index_usage.txt", PostgresIndexUsageFileName)
	assert.Equal(t, "pg_tablespaces.txt", PostgresTablespacesFileName)
	assert.Equal(t, "pg_checkpoint_log.txt", PostgresCheckpointLogFileName)
	assert.Equal(t, "pg_xid_age.txt", PostgresXIDAgeFileName)
	assert.Equal(t, "pg_nondefault_settings.txt", PostgresNonDefaultSettingsFileName)
	assert.Equal(t, "pg_catalog_map.txt", PostgresCatalogMapFileName)
	assert.Equal(t, "pg_memory.txt", PostgresMemoryFileName)
	assert.Equal(t, "pg_errors.txt", PostgresErrorsFileName)

	seen := map[string]bool{}
	for _, name := range postgresArtifactFiles {
		assert.True(t, strings.HasPrefix(name, "pg_"),
			"%s: database artifacts keep engine-specific prefixes, pg_ here", name)

		assert.NotContains(t, name, ".appLogs.",
			"%s is classified by its exact name, not by the app-log substring convention", name)

		assert.False(t, seen[name], "%s is used for two artifacts", name)
		seen[name] = true
	}

	assert.Len(t, seen, 18, "every artifact the run writes is named here")
}

func TestPostgresSampledDataTypeGate(t *testing.T) {
	assert.Equal(t, pgDTMetadata, pgSampledDataType(pgMetadataCollector().Artifact()),
		"pg_metadata.txt is routed through the same gate as every other artifact")

	assert.Equal(t, pgDTBloat, pgSampledDataType(postgres.Bloat{}.Artifact()),
		"pg_bloat.txt has its assigned value")

	assert.Equal(t, pgDTHealth, pgSampledDataType(postgres.Health{}.Artifact()),
		"pg_health.txt has its assigned value")

	assert.Equal(t, pgDTCapacity, pgSampledDataType(postgres.Capacity{}.Artifact()),
		"pg_capacity.txt has its assigned value")

	assert.Equal(t, pgDTReplication, pgSampledDataType(postgres.Replication{}.Artifact()),
		"pg_replication.txt has its assigned value")

	assert.Equal(t, pgDTSlowQueries, pgSampledDataType(postgres.NewSlowQueries().Artifact()),
		"pg_slow_queries.txt has its assigned value")

	assert.Equal(t, pgDTSessions, pgSampledDataType(postgres.Sessions{}.Artifact()),
		"pg_sessions.txt has its assigned value - the last of the nine to get one")

	assert.Equal(t, pgDTDeadlocks, pgSampledDataType(postgres.NewDeadlocks().Artifact()),
		"and so does pg_deadlocks.txt, which is written in Mode H only and uploaded like any "+
			"other when it is")

	assert.Equal(t, pgDTTimeouts, pgSampledDataType(postgres.NewTimeouts().Artifact()),
		"and pg_timeouts.txt, on the same terms")

	assert.Equal(t, pgDTExplain,
		pgSampledDataType(postgres.NewExplain("", postgres.NewSlowQueries()).Artifact()),
		"pg_explain.txt has its assigned value - the tenth and last, which closes the gate "+
			"this switch existed to hold open")

	assert.Equal(t, pgDTIndexUsage, pgSampledDataType(postgres.IndexUsage{}.Artifact()),
		"pg_index_usage.txt uploads under the value the agent proposed, ahead of the "+
			"receiver knowing it")

	assert.Equal(t, pgDTTablespaces, pgSampledDataType(postgres.Tablespaces{}.Artifact()),
		"pg_tablespaces.txt on the same terms")

	assert.Equal(t, pgDTCheckpointLog, pgSampledDataType(postgres.NewCheckpointLog().Artifact()),
		"and pg_checkpoint_log.txt")

	assert.Equal(t, pgDTXIDAge, pgSampledDataType(postgres.XIDAge{}.Artifact()),
		"and pg_xid_age.txt")

	assert.Equal(t, pgDTNonDefaultSettings, pgSampledDataType(postgres.NonDefaultSettings{}.Artifact()),
		"and pg_nondefault_settings.txt")

	assert.Equal(t, pgDTCatalogMap, pgSampledDataType(postgres.CatalogMap{}.Artifact()),
		"and pg_catalog_map.txt")

	assert.Equal(t, pgDTMemory, pgSampledDataType(postgres.Memory{}.Artifact()),
		"and pg_memory.txt")

	assert.Equal(t, pgDTErrors, pgSampledDataType(postgres.NewErrors().Artifact()),
		"and pg_errors.txt, under a dt of its own rather than as an application log")

	assert.Empty(t, pgSampledDataType(postgres.Artifact{Name: "pg_future"}),
		"and an artifact with no dt at all is still refused rather than guessed at - the "+
			"rule a future artifact lands under until its value exists")
}

func pgMetadataCollector() *postgres.MetadataCollector {
	return postgres.NewMetadata(postgres.Target{}, "3.6.1", time.Now(), "")
}

func TestPostgresMetadataFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresMetadataFileName, pgMetadataCollector().Artifact().FileName)
}

func TestPostgresBloatFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresBloatFileName, postgres.Bloat{}.Artifact().FileName)
}

func TestPostgresHealthFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresHealthFileName, postgres.Health{}.Artifact().FileName)
}

func TestPostgresCapacityFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresCapacityFileName, postgres.Capacity{}.Artifact().FileName)
}

func TestPostgresReplicationFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresReplicationFileName, postgres.Replication{}.Artifact().FileName)
}

func TestPostgresSessionsFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresSessionsFileName, postgres.Sessions{}.Artifact().FileName)
}

func TestPostgresSlowQueriesFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresSlowQueriesFileName, postgres.NewSlowQueries().Artifact().FileName)
}

func TestPostgresIndexUsageFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresIndexUsageFileName, postgres.IndexUsage{}.Artifact().FileName)
}

func TestPostgresTablespacesFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresTablespacesFileName, postgres.Tablespaces{}.Artifact().FileName)
}

func TestPostgresCheckpointLogFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresCheckpointLogFileName, postgres.NewCheckpointLog().Artifact().FileName)
}

func TestPostgresXIDAgeFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresXIDAgeFileName, postgres.XIDAge{}.Artifact().FileName)
}

func TestPostgresNonDefaultSettingsFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresNonDefaultSettingsFileName, postgres.NonDefaultSettings{}.Artifact().FileName)
}

func TestPostgresCatalogMapFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresCatalogMapFileName, postgres.CatalogMap{}.Artifact().FileName)
}

func TestPostgresMemoryFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresMemoryFileName, postgres.Memory{}.Artifact().FileName)
}

func TestPostgresErrorsFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresErrorsFileName, postgres.NewErrors().Artifact().FileName)
}

func TestPostgresSlowQueriesReachesTheClosingTick(t *testing.T) {
	require.Equal(t, postgres.Periodic(0), postgres.NewSlowQueries().Artifact().Schedule,
		"a periodic collector, so its last offset is exactly the closing tick "+
			"and its budget joins capacity's and bloat's there")

	assert.Equal(t, 3*postgres.StatementTimeout, postgres.NewSlowQueries().Artifact().SampleBudget,
		"a preflight and two reads, declared rather than under-stated: buying a shorter "+
			"deadline by lying about the arithmetic is the one thing this number must not do")
}

func TestPostgresSampledCollectorsShareOneCadence(t *testing.T) {
	// Two minutes at 30s, five samples. The 5m default on the default window is
	// the bookend alone, and Validate warns about it.
	interval := 30 * time.Second

	for name, schedule := range map[string]postgres.Schedule{
		"pg_sessions":            postgres.Sessions{Interval: interval}.Artifact().Schedule,
		"pg_health":              postgres.Health{Interval: interval}.Artifact().Schedule,
		"pg_xid_age":             postgres.XIDAge{Interval: interval}.Artifact().Schedule,
		"pg_replication":         postgres.Replication{Interval: interval}.Artifact().Schedule,
		"pg_nondefault_settings": postgres.NonDefaultSettings{Interval: interval}.Artifact().Schedule,
		"pg_memory":              postgres.Memory{Interval: interval}.Artifact().Schedule,
		"pg_capacity":            postgres.Capacity{Interval: interval}.Artifact().Schedule,
		"pg_bloat":               postgres.Bloat{Interval: interval}.Artifact().Schedule,
		"pg_index_usage":         postgres.IndexUsage{Interval: interval}.Artifact().Schedule,
		"pg_tablespaces":         postgres.Tablespaces{Interval: interval}.Artifact().Schedule,
		"pg_slow_queries":        (&postgres.SlowQueries{Interval: interval}).Artifact().Schedule,
	} {
		assert.Equal(t, postgres.Periodic(interval), schedule, name+
			" takes the run's cadence, not a constant or a bookend of its own")
	}

	assert.Equal(t, postgres.Every(postgres.DefaultLogTailInterval),
		postgres.NewDeadlocks().Artifact().Schedule,
		"and the log tails do not: their poll bounds what a cancelled window loses, "+
			"which is not a sampling rate")
	assert.Equal(t, postgres.Every(postgres.DefaultLogTailInterval),
		postgres.NewCheckpointLog().Artifact().Schedule,
		"the third tail, on the same terms as its two siblings")

	explain := postgres.NewExplain(postgres.ExplainModeLogged, postgres.NewSlowQueries())
	explain.Interval = interval
	assert.Equal(t, postgres.Periodic(interval), explain.Artifact().Schedule,
		"and pg_explain takes it too, since the once-per-shape rework: every sample walks "+
			"that tick's statements read for shapes not yet seen")
}

func TestPostgresBookendCollectorsDeclareTheirClosingBudgets(t *testing.T) {
	sessions := postgres.Sessions{}.Artifact()
	health := postgres.Health{}.Artifact()
	replication := postgres.Replication{}.Artifact()

	assert.Equal(t, 3*time.Second, sessions.SampleBudget,
		"two statements at this collector's own timeout, declared so the next reader "+
			"computing the deadline by hand does not have to derive it")
	assert.Equal(t, postgres.StatementTimeout, health.SampleBudget,
		"one statement: left at zero, DefaultSampleBudget would charge the tick for two")
	assert.Zero(t, replication.SampleBudget,
		"two statements is DefaultSampleBudget already, so there is nothing to restate")

	assert.Equal(t, postgres.ConnectionFast, sessions.Connection)
	assert.Equal(t, postgres.ConnectionNormal, health.Connection)
	assert.Equal(t, postgres.ConnectionNormal, replication.Connection)

	assert.Equal(t, 15*time.Second, health.SampleBudget+postgres.DefaultSampleBudget,
		"what the bookend costs the normal connection's closing tick: health and replication "+
			"land on it, one after the other. Sessions' 3s is on the fast connection, beside it")
}

func TestPostgresCapacityDeclaresTheClosingTicksBudget(t *testing.T) {
	capacity := postgres.Capacity{}.Artifact()
	bloat := postgres.Bloat{}.Artifact()

	require.Equal(t, postgres.Periodic(0), capacity.Schedule,
		"both land on the closing tick: Periodic's last sample is the close")
	require.Equal(t, postgres.Periodic(0), bloat.Schedule)

	assert.Equal(t, 5*postgres.StatementTimeout, capacity.SampleBudget,
		"five statements, the WAL read's privilege check among them: left at zero, the "+
			"normal connection's closing tick would be sized for two")
	assert.Zero(t, bloat.SampleBudget, "bloat's two statements are the default shape")

	assert.Equal(t, postgres.ConnectionNormal, capacity.Connection)
	assert.Equal(t, postgres.ConnectionExpensive, bloat.Connection,
		"so bloat's closing read runs beside capacity's, not after it")

	assert.Equal(t, 25*time.Second, capacity.SampleBudget,
		"what capacity costs its connection's closing tick - a load commitment against a "+
			"database already in trouble, and one that should move only deliberately")
}

func TestPostgresIndexUsageJoinsTheClosingTick(t *testing.T) {
	indexUsage := postgres.IndexUsage{}.Artifact()

	require.Equal(t, postgres.Periodic(0), indexUsage.Schedule,
		"born periodic, so its last sample is the close")
	assert.Zero(t, indexUsage.SampleBudget, "two statements, the default shape, copied from bloat")
	assert.Equal(t, postgres.ConnectionExpensive, indexUsage.Connection)

	assert.Equal(t, 10*time.Second, postgres.DefaultSampleBudget,
		"what it adds to the expensive connection's closing tick: one more DefaultSampleBudget, "+
			"after bloat's")
}

func TestPostgresTablespacesJoinsTheClosingTick(t *testing.T) {
	tablespaces := postgres.Tablespaces{}.Artifact()

	require.Equal(t, postgres.Periodic(0), tablespaces.Schedule, "born periodic, so its last sample is the close")
	assert.Equal(t, postgres.StatementTimeout, tablespaces.SampleBudget,
		"one statement, declared: what the twelfth artifact adds to the closing tick is 10s, "+
			"not the 20s a zero would have charged it")
}

func TestPostgresXIDAgeJoinsTheClosingTick(t *testing.T) {
	xidAge := postgres.XIDAge{}.Artifact()

	require.Equal(t, postgres.Periodic(0), xidAge.Schedule, "born periodic, so its last sample is the close")
	assert.Equal(t, postgres.StatementTimeout, xidAge.SampleBudget,
		"one statement, declared: what this artifact adds to the closing tick is 10s")
}

func TestPostgresMemoryJoinsTheClosingTick(t *testing.T) {
	memory := postgres.Memory{}.Artifact()

	require.Equal(t, postgres.Periodic(0), memory.Schedule, "born periodic, so its last sample is the close")
	assert.Zero(t, memory.SampleBudget,
		"the privilege check and the read, the default shape: what this artifact adds to "+
			"the closing tick is 20s")
}

func TestPostgresNonDefaultSettingsJoinsTheClosingTick(t *testing.T) {
	settings := postgres.NonDefaultSettings{}.Artifact()

	require.Equal(t, postgres.Periodic(0), settings.Schedule, "born periodic, so its last sample is the close")
	assert.Equal(t, postgres.StatementTimeout, settings.SampleBudget,
		"one statement, declared: what this artifact adds to the closing tick is 10s")
}

func pgCollectedMetadata() postgres.Metadata {
	return postgres.Metadata{
		LogAccess: postgres.LogAccessDirect,

		// direct beside no is the measured pairing from the fixture matrix: a
		// bind-mounted log directory is readable from a machine that is not the
		// database's.
		AgentOnDBHost: postgres.OnDBHostNo,
	}
}

func TestPostgresResultMessage(t *testing.T) {
	tests := []struct {
		name     string
		metadata postgres.Metadata
		want     string
	}{
		{
			name:     "collected",
			metadata: pgCollectedMetadata(),
			want:     "pg_metadata.txt written (log_access=direct, agent_on_db_host=no)",
		},
		{
			name: "connection refused",
			metadata: postgres.Metadata{
				LogAccess:    postgres.LogAccessUnknown,
				ConnectError: "failed to connect to `host=127.0.0.1`: connection refused",
			},
			want: "pg_metadata.txt written; postgres connect failed: " +
				"failed to connect to `host=127.0.0.1`: connection refused",
		},
		{

			name: "database at max_connections",
			metadata: postgres.Metadata{
				LogAccess:    postgres.LogAccessUnknown,
				ConnectError: "too_many_connections",
			},
			want: "pg_metadata.txt written; postgres connect failed: too_many_connections",
		},
		{

			name: "a probe was denied",
			metadata: postgres.Metadata{
				LogAccess:           postgres.LogAccessUnknown,
				AgentOnDBHost:       postgres.OnDBHostUnknown,
				CurrentLogfileError: "ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
			},
			want: "pg_metadata.txt written (log_access=unknown, agent_on_db_host=unknown); " +
				"pg_current_logfile failed: " +
				"ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
		},
		{
			name: "the function was not called",
			metadata: postgres.Metadata{
				LogAccess:             postgres.LogAccessNone,
				AgentOnDBHost:         postgres.OnDBHostNo,
				CurrentLogfileSkipped: true,
			},
			want: "pg_metadata.txt written (log_access=none, agent_on_db_host=no); " +
				"pg_current_logfile not called: this role may not execute it",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, postgresResultMessage(tt.metadata))
		})
	}
}

func withWindow(t *testing.T, d time.Duration) *config.Postgres {
	t.Helper()

	target := unreachablePostgres(t)
	window := config.Duration(d)
	target.CaptureDuration = &window

	return target
}

func readBloatArtifact(t *testing.T) string {
	t.Helper()

	return readSampledArtifact(t, PostgresBloatFileName)
}

func readSampledArtifact(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(name)
	require.NoError(t, err)

	return string(content)
}

var postgresArtifactFiles = []string{
	PostgresDeadlocksFileName,
	PostgresTimeoutsFileName,
	PostgresCheckpointLogFileName,
	PostgresErrorsFileName,
	PostgresSessionsFileName,
	PostgresHealthFileName,
	PostgresXIDAgeFileName,
	PostgresReplicationFileName,
	PostgresNonDefaultSettingsFileName,
	PostgresMemoryFileName,
	PostgresMetadataFileName,
	PostgresCatalogMapFileName,
	PostgresCapacityFileName,
	PostgresBloatFileName,
	PostgresIndexUsageFileName,
	PostgresTablespacesFileName,
	PostgresSlowQueriesFileName,
	PostgresExplainFileName,
}

// postgresArtifactsAwaitingDT are written into the bundle and held back from
// upload until a value exists. Empty: every shipped artifact has one. An entry
// here is one line in pgSampledDataType when its value arrives.
var postgresArtifactsAwaitingDT []string

func TestPostgresCaptureRunUnreachableTarget(t *testing.T) {
	dir := chdirToCaptureDir(t)

	task := &PostgresCapture{Target: withWindow(t, 2*time.Minute)}

	started := time.Now()
	result, err := task.Run()
	elapsed := time.Since(started)

	require.NoError(t, err, "a refused connection is not a capture failure")

	for _, name := range postgresArtifactFiles {
		assert.FileExists(t, filepath.Join(dir, name))

		artifact := readSampledArtifact(t, name)
		assert.Contains(t, artifact, "status=started",
			"%s: the preamble is written before connecting", name)
		assert.Contains(t, artifact, "status=connect_failed", name)
		assert.Contains(t, artifact, "samples_written=0", name)
	}

	assert.Less(t, elapsed, 30*time.Second,
		"a connect failure waited out the window instead of failing fast")

	assert.Contains(t, result.Msg, PostgresSessionsFileName+" written (0/9 samples)",
		"sessions sample at the fast speed, at most 15s, whatever the frequency: 2m at 15s "+
			"is eight steps and the close - and it reports a refusal like every other")
	assert.Contains(t, result.Msg, PostgresHealthFileName+" written (0/2 samples)",
		"the 5m default on the 2m window is the bookend alone at the normal speed; Validate "+
			"is what warns a deployment about the two samples, since the capture takes what it is given")
	assert.Contains(t, result.Msg, PostgresReplicationFileName+" written (0/2 samples)",
		"and the same again for replication, at the normal speed")
	assert.Contains(t, result.Msg, PostgresBloatFileName+" written (0/2 samples)",
		"the whole-table reads at the expensive speed, at least 5m")
	assert.Contains(t, result.Msg, PostgresCapacityFileName+" written (0/2 samples)")
	assert.Contains(t, result.Msg, PostgresIndexUsageFileName+" written (0/2 samples); postgres connect failed",
		"the eleventh artifact takes the cadence too, and reports the refusal like the rest")
	assert.Contains(t, result.Msg, PostgresTablespacesFileName+" written (0/2 samples); postgres connect failed",
		"and the twelfth")
	assert.Contains(t, result.Msg, PostgresCheckpointLogFileName+" written (0/12 samples); postgres connect failed",
		"and the third log tail, on its siblings' 10s poll rather than the cadence")
	assert.Contains(t, result.Msg, PostgresXIDAgeFileName+" written (0/2 samples); postgres connect failed",
		"and pg_xid_age.txt, on the cadence")
	assert.Contains(t, result.Msg, PostgresNonDefaultSettingsFileName+" written (0/2 samples); postgres connect failed",
		"and pg_nondefault_settings.txt")
	assert.Contains(t, result.Msg, PostgresCatalogMapFileName+" written (0/1 samples); postgres connect failed",
		"and pg_catalog_map.txt, read once")
	assert.Contains(t, result.Msg, PostgresMemoryFileName+" written (0/2 samples); postgres connect failed",
		"and pg_memory.txt, on the cadence")
	assert.Equal(t, len(postgresArtifactsAwaitingDT), strings.Count(result.Msg, "; not uploaded: dt value not yet assigned"),
		"and exactly the held-back ones say why, after the refusal rather than instead of it")
	assert.Contains(t, result.Msg, PostgresSlowQueriesFileName+" written (0/2 samples)")
	assert.Contains(t, result.Msg, PostgresExplainFileName+" written (0/2 samples)",
		"and pg_explain takes the cadence too: every sample walks that tick's statements read")
	assert.Contains(t, result.Msg, PostgresMetadataFileName+" written; postgres connect failed",
		"every artifact reports the one refusal, and they report it identically")
	assert.Contains(t, result.Msg, PostgresErrorsFileName+" written (0/12 samples); postgres connect failed",
		"and the fourth log tail, on the same 10s poll")
	assert.Less(t, strings.Index(result.Msg, PostgresCheckpointLogFileName),
		strings.Index(result.Msg, PostgresErrorsFileName), "after the other three tails")
	assert.Less(t, strings.Index(result.Msg, PostgresErrorsFileName),
		strings.Index(result.Msg, PostgresSessionsFileName), "and before the first SQL read")

	assert.Less(t, strings.Index(result.Msg, PostgresCapacityFileName),
		strings.Index(result.Msg, PostgresBloatFileName),
		"capacity samples before bloat on every shared tick: registration order is sampling "+
			"order, and the run's message lists the artifacts in it")
	assert.Less(t, strings.Index(result.Msg, PostgresMetadataFileName),
		strings.Index(result.Msg, PostgresCatalogMapFileName),
		"the catalog map is read at the start, right after pg_metadata")
	assert.Less(t, strings.Index(result.Msg, PostgresCatalogMapFileName),
		strings.Index(result.Msg, PostgresCapacityFileName),
		"and before the whole-table reads")
	assert.Less(t, strings.Index(result.Msg, PostgresBloatFileName),
		strings.Index(result.Msg, PostgresIndexUsageFileName),
		"and bloat before index usage: the table reading before the reading that joins to it")

	_, values := readPostgresArtifact(t)
	assert.Equal(t, "127.0.0.1", values["target_host"],
		"the target block is on disk even though the connection never happened")
	assert.NotContains(t, values, "current_database",
		"and the server block is not, because there was no server to read")
}

func TestPostgresCaptureOpensOneConnectionPerRun(t *testing.T) {
	chdirToCaptureDir(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu       sync.Mutex
		accepted []net.Conn
	)

	t.Cleanup(func() {
		listener.Close()

		mu.Lock()
		defer mu.Unlock()

		for _, conn := range accepted {
			conn.Close()
		}
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()

	target := withWindow(t, time.Second)
	target.Host = "127.0.0.1"
	target.Port = listener.Addr().(*net.TCPAddr).Port
	target.TLS = &config.PostgresTLS{Enabled: new(bool)}

	_, err = (&PostgresCapture{Target: target}).Run()
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	assert.Len(t, accepted, 1, "one run, one connection, whatever it writes through it")
}

func TestPostgresCaptureUploadsUnderAssignedDT(t *testing.T) {
	chdirToCaptureDir(t)

	previous := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.OnlyCapture = false
	t.Cleanup(func() { config.GlobalConfig.OnlyCapture = previous })

	var (
		mu      sync.Mutex
		uploads []recordedPostgresUpload
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		uploads = append(uploads, recordedPostgresUpload{
			dt:   r.URL.Query().Get("dt"),
			body: string(body),
		})
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(server.Close)

	task := &PostgresCapture{Target: withWindow(t, time.Second)}
	task.SetEndpoint(server.URL + "?de=test")

	result, err := task.Run()
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, uploads, len(postgresArtifactFiles)-len(postgresArtifactsAwaitingDT),
		"every artifact with a dt is uploaded")

	byDT := map[string]string{}
	for _, upload := range uploads {
		byDT[upload.dt] = upload.body
	}

	require.Contains(t, byDT, pgDTMetadata, "pg_metadata.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTHealth, "pg_health.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTBloat, "pg_bloat.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTCapacity, "pg_capacity.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTReplication, "pg_replication.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTSessions, "pg_sessions.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTSlowQueries, "pg_slow_queries.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTDeadlocks, "pg_deadlocks.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTTimeouts, "pg_timeouts.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTExplain, "pg_explain.txt uploaded under the wrong dt")

	for dt, source := range map[string]string{
		pgDTMetadata:           "source=pg_metadata",
		pgDTHealth:             "source=pg_health",
		pgDTBloat:              "source=pg_bloat",
		pgDTCapacity:           "source=pg_capacity",
		pgDTReplication:        "source=pg_replication",
		pgDTSessions:           "source=pg_sessions",
		pgDTSlowQueries:        "source=pg_slow_queries",
		pgDTDeadlocks:          "source=pg_deadlocks",
		pgDTTimeouts:           "source=pg_timeouts",
		pgDTExplain:            "source=pg_explain",
		pgDTIndexUsage:         "source=pg_index_usage",
		pgDTTablespaces:        "source=pg_tablespaces",
		pgDTCheckpointLog:      "source=pg_checkpoint_log",
		pgDTXIDAge:             "source=pg_xid_age",
		pgDTNonDefaultSettings: "source=pg_nondefault_settings",
		pgDTCatalogMap:         "source=pg_catalog_map",
		pgDTMemory:             "source=pg_memory",
		pgDTErrors:             "source=pg_errors",
	} {
		assert.Contains(t, byDT[dt], source, "dt=%s carried another artifact's body", dt)
		assert.Contains(t, byDT[dt], "status=connect_failed",
			"dt=%s: a capture of a failure is uploaded like any other", dt)
		assert.NotContains(t, byDT[dt], pgTestPassword, "dt=%s: the upload carries the password", dt)
	}

	for _, name := range postgresArtifactFiles {
		assert.Contains(t, result.Msg, name+" written",
			"%s: the transmission message displaced the capture summary", name)
		assert.FileExists(t, name, "%s: an uploaded artifact is still written into the bundle", name)
	}

	assert.Equal(t, len(postgresArtifactFiles)-1, strings.Count(result.Msg, " | "),
		"one summary per artifact written, joined into one run-level record")

	assert.NotContains(t, byDT, "pgRepl",
		"and none of them under the abbreviation the replication slice proposed and the server "+
			"team did not take: classification is an exact string match, so the near miss is "+
			"dropped with no error at either end")

	assert.NotContains(t, byDT, "",
		"nothing posted under an empty dt: an invented value uploads and is then dropped "+
			"silently at the far end, which is why the gate skips instead of guessing")

	assert.Len(t, byDT, len(postgresArtifactFiles),
		"every artifact the run writes goes up under a dt of its own, the newest under the "+
			"values the agent proposed")
	assert.Equal(t, len(postgresArtifactsAwaitingDT),
		strings.Count(result.Msg, "; not uploaded: dt value not yet assigned"),
		"nothing is held back while no artifact lacks a value; the rule stays for one that does")

	assert.True(t, result.Ok,
		"the run's Ok is spent only on an artifact held back or refused, and there is none")
}

func TestPostgresCaptureMetadataLineKeepsItsProbeClauses(t *testing.T) {
	collected := postgres.Metadata{
		LogAccess:           postgres.LogAccessNone,
		AgentOnDBHost:       postgres.OnDBHostNo,
		CurrentLogfileError: "ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
	}

	artifact := postgres.ArtifactResult{
		Artifact:        pgMetadataCollector().Artifact(),
		Status:          postgres.StatusComplete,
		SamplesExpected: 1,
		SamplesWritten:  1,
	}

	summary := postgresArtifactSummary(artifact, collected)

	assert.Contains(t, summary, PostgresMetadataFileName+" written (log_access=none, agent_on_db_host=no)",
		"the two readings, not the count")
	assert.Contains(t, summary, "pg_current_logfile failed",
		"and the probe clause, which a sample count would have swallowed whole")
	assert.NotContains(t, summary, "samples")

	artifact.Status = postgres.StatusConnectFailed
	artifact.SamplesWritten = 0
	artifact.Err = "too_many_connections"

	assert.Equal(t,
		PostgresMetadataFileName+" written; postgres connect failed: too_many_connections",
		postgresArtifactSummary(artifact, postgres.Metadata{LogAccess: postgres.LogAccessUnknown}),
		"a refusal reaches this line through the window, since the collector never ran")

	assert.Equal(t, "pg_bloat.txt written (2/2 samples)", postgresArtifactSummary(
		postgres.ArtifactResult{
			Artifact:        postgres.Bloat{}.Artifact(),
			Status:          postgres.StatusComplete,
			SamplesExpected: 2, SamplesWritten: 2,
		}, collected), "every other artifact keeps the count")
}

func TestPostgresCaptureIOFailureOnOneArtifactSparesTheOthers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory does not block os.Create on Windows the way it does here")
	}

	chdirToCaptureDir(t)

	require.NoError(t, os.Mkdir(PostgresMetadataFileName, 0o755))

	task := &PostgresCapture{Target: withWindow(t, time.Second)}

	_, err := task.Run()
	require.Error(t, err, "an I/O failure is the one failure an artifact cannot record about itself")
	assert.Contains(t, err.Error(), PostgresMetadataFileName)

	for _, name := range []string{PostgresHealthFileName, PostgresBloatFileName} {
		assert.FileExists(t, name,
			"%s: one artifact's I/O failure must not cost another its capture", name)
		assert.Contains(t, readSampledArtifact(t, name), "status=connect_failed", name)
	}
}

func TestPostgresCaptureRedactsPasswordInEveryArtifact(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresCapture{Target: withWindow(t, time.Second)}

	result, err := task.Run()
	require.NoError(t, err)

	for _, name := range postgresArtifactFiles {
		assert.NotContains(t, readSampledArtifact(t, name), pgTestPassword,
			"%s carries the password", name)
	}

	assert.NotContains(t, result.Msg, pgTestPassword, "the result message carries the password")
}

func TestPostgresCaptureRedactsPasswordOnConnectFailure(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresCapture{Target: withWindow(t, time.Second)}

	result, err := task.Run()
	require.NoError(t, err)

	assert.NotContains(t, readBloatArtifact(t), pgTestPassword,
		"the artifact carries the password")
	assert.NotContains(t, result.Msg, pgTestPassword, "the result message carries the password")

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(verb, task), pgTestPassword,
			"the task leaked the password through %s", verb)
	}
}

func TestPostgresCaptureWrapRunFailureCarriesNoPassword(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions do not deny creation on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root; a read-only directory would not deny creation")
	}

	dir := chdirToCaptureDir(t)
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	log := captureLogOutput(t)

	task := &PostgresCapture{Target: withWindow(t, time.Second)}

	results := make(chan Result, 1)
	WrapRun(task)("http://localhost/endpoint", results)

	result := <-results
	assert.Contains(t, result.Msg, "capture failed", "the I/O failure is a real capture failure")

	logged := log.String()
	require.Contains(t, logged, "capture", "the failure was not logged; the test proves nothing")
	assert.Contains(t, logged, "config.Postgres{",
		"%#v did not reach GoString; the redaction below is vacuous")
	assert.NotContains(t, logged, pgTestPassword, "WrapRun logged the password")
}

func TestPostgresCaptureNilTarget(t *testing.T) {
	chdirToCaptureDir(t)

	result, err := (&PostgresCapture{}).Run()
	require.NoError(t, err)

	assert.Contains(t, result.Msg, "no postgres block configured")
	for _, name := range postgresArtifactFiles {
		assert.NoFileExists(t, name)
	}
}

func TestPostgresCaptureKillCancelsAnInFlightWindow(t *testing.T) {
	chdirToCaptureDir(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		acceptedMu sync.Mutex
		accepted   []net.Conn
	)

	t.Cleanup(func() {
		listener.Close()

		acceptedMu.Lock()
		defer acceptedMu.Unlock()

		for _, conn := range accepted {
			conn.Close()
		}
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			acceptedMu.Lock()
			accepted = append(accepted, conn)
			acceptedMu.Unlock()
		}
	}()

	target := withWindow(t, 2*time.Minute)
	target.Host = "127.0.0.1"
	target.Port = listener.Addr().(*net.TCPAddr).Port
	target.TLS = &config.PostgresTLS{Enabled: new(bool)}

	task := &PostgresCapture{Target: target}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, runErr := task.Run()
		assert.NoError(t, runErr)
	}()

	time.Sleep(250 * time.Millisecond)

	select {
	case <-done:
		t.Fatal("Run finished before Kill was called; the test proves nothing")
	default:
	}

	require.NoError(t, task.Kill())

	started := time.Now()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Kill did not stop the window")
	}

	assert.Less(t, time.Since(started), 4*time.Second,
		"Kill did not interrupt the connect; it ran to ConnectTimeout or the window")

	assert.Contains(t, readBloatArtifact(t), "samples_written=0")
}

func TestPostgresCaptureKillOutsideARun(t *testing.T) {
	task := &PostgresCapture{Target: unreachablePostgres(t)}

	assert.NoError(t, task.Kill(), "Kill before Run must not panic")

	chdirToCaptureDir(t)
	_, err := task.Run()
	require.NoError(t, err)

	assert.NoError(t, task.Kill(), "Kill after Run must not panic")
}

func TestPostgresCaptureDefaultsTheWindowWhenUnvalidated(t *testing.T) {
	task := &PostgresCapture{Target: unreachablePostgres(t)}
	require.Nil(t, task.Target.CaptureDuration)

	assert.Equal(t, unvalidatedCaptureDuration, task.captureDuration(),
		"Validate refuses a block without the key; one built in code still gets a window")

	window := config.Duration(45 * time.Second)
	task.Target.CaptureDuration = &window
	assert.Equal(t, 45*time.Second, task.captureDuration())
}

func TestPostgresCaptureDefaultsTheFrequencyWhenUnvalidated(t *testing.T) {
	task := &PostgresCapture{Target: unreachablePostgres(t)}
	require.Nil(t, task.Target.Frequency)

	assert.Equal(t, config.DefaultPostgresFrequency, task.frequency(),
		"omitted takes 5m, the same value Validate would have filled in")

	frequency := config.Duration(30 * time.Second)
	task.Target.Frequency = &frequency
	assert.Equal(t, 30*time.Second, task.frequency())

	zero := config.Duration(0)
	task.Target.Frequency = &zero
	assert.Equal(t, config.DefaultPostgresFrequency, task.frequency(),
		"a zero that never went through Validate takes the default rather than sampling nothing")
}

func TestPostgresFrequencyFloorIsTheStatementTimeout(t *testing.T) {
	assert.Equal(t, postgres.StatementTimeout, config.MinPostgresFrequency,
		"config floors frequency at the capture's per-statement timeout, so a maxed-out "+
			"sample can never outrun the tick behind it; the two packages do not import each "+
			"other, so the spellings are pinned equal here")
}

func TestPostgresCaptureHonoursTheConfiguredFrequency(t *testing.T) {
	chdirToCaptureDir(t)

	target := withWindow(t, 2*time.Minute)
	frequency := config.Duration(30 * time.Second)
	target.Frequency = &frequency

	result, err := (&PostgresCapture{Target: target}).Run()
	require.NoError(t, err)

	assert.Contains(t, result.Msg, PostgresHealthFileName+" written (0/5 samples)",
		"2m at 30s: four steps and the close, where the "+
			"5m default would have been the bookend alone")
	assert.Contains(t, result.Msg, PostgresSlowQueriesFileName+" written (0/5 samples)")
	assert.Contains(t, result.Msg, PostgresExplainFileName+" written (0/5 samples)",
		"and pg_explain takes the same five: it walks every sample's statements read")
}

func TestPostgresCaptureSamplesEachFileAtItsSpeed(t *testing.T) {
	chdirToCaptureDir(t)

	target := withWindow(t, 10*time.Minute)
	frequency := config.Duration(time.Minute)
	target.Frequency = &frequency

	result, err := (&PostgresCapture{Target: target}).Run()
	require.NoError(t, err)

	// 10m at 1m: fast 15s is forty steps and the close; normal 1m is ten and the
	// close; expensive 5m is two and the close.
	for _, tt := range []struct {
		file    string
		samples int
	}{
		{PostgresSessionsFileName, 41},
		{PostgresHealthFileName, 11},
		{PostgresXIDAgeFileName, 11},
		{PostgresReplicationFileName, 11},
		{PostgresMemoryFileName, 11},
		{PostgresCapacityFileName, 11},
		{PostgresSlowQueriesFileName, 11},
		{PostgresExplainFileName, 11},
		{PostgresBloatFileName, 3},
		{PostgresIndexUsageFileName, 3},
		{PostgresTablespacesFileName, 3},
		{PostgresNonDefaultSettingsFileName, 3},
		{PostgresCatalogMapFileName, 1},
		{PostgresDeadlocksFileName, 60},
	} {
		assert.Contains(t, result.Msg, fmt.Sprintf("%s written (0/%d samples)", tt.file, tt.samples))
	}
}

func TestPostgresCaptureMessage(t *testing.T) {
	artifact := postgres.Bloat{}.Artifact()

	tests := []struct {
		name   string
		result postgres.ArtifactResult
		want   string
	}{
		{
			name: "complete",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusComplete,
				SamplesExpected: 2, SamplesWritten: 2,
			},
			want: "pg_bloat.txt written (2/2 samples)",
		},
		{
			name: "connect failed",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusConnectFailed,
				SamplesExpected: 2, Err: "too_many_connections",
			},
			want: "pg_bloat.txt written (0/2 samples); postgres connect failed: too_many_connections",
		},
		{
			name: "partial",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusPartial,
				SamplesExpected: 2, SamplesWritten: 1,
				Err: "ERROR: canceling statement due to statement timeout",
			},
			want: "pg_bloat.txt written (1/2 samples); last sample error: " +
				"ERROR: canceling statement due to statement timeout",
		},
		{
			name: "cancelled",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusCancelled,
				SamplesExpected: 2, SamplesWritten: 1,
			},
			want: "pg_bloat.txt written (1/2 samples); window cancelled",
		},
		{
			name: "deadline exceeded",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusDeadlineExceeded,
				SamplesExpected: 2, SamplesWritten: 1,
			},
			want: "pg_bloat.txt written (1/2 samples); window deadline exceeded",
		},
		{
			name: "connection lost",
			result: postgres.ArtifactResult{
				Artifact: artifact, Status: postgres.StatusConnectionLost,
				SamplesExpected: 2, SamplesWritten: 1,
				Err: "FATAL: terminating connection due to administrator command (SQLSTATE 57P01)",
			},
			want: "pg_bloat.txt written (1/2 samples); connection lost: " +
				"FATAL: terminating connection due to administrator command (SQLSTATE 57P01)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, postgresArtifactMessage(tt.result))
		})
	}
}
