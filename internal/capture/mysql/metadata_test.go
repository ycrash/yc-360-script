package mysql

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metadataServer answers my_metadata.txt's statements as a server would.
func metadataServer(version string, logError string) *fakeQuerier {
	return (&fakeQuerier{}).
		on("SELECT VERSION(), @@version_comment, CURRENT_USER()", fakeResult{
			columns: []string{"v", "c", "u", "id", "db", "utc"},
			rows:    [][]any{{version, "MySQL Community Server - GPL", "yc_monitor@%", 42, nil, "2026-10-27T09:00:00.123456Z"}},
		}).
		on("SHOW GLOBAL VARIABLES", fakeResult{
			columns: []string{"Variable_name", "Value"},
			rows: [][]any{
				{"datadir", "/var/lib/mysql/"},
				{"log_error", logError},
				{"innodb_print_all_deadlocks", "ON"},
				{"performance_schema", "ON"},
			},
		}).
		on("SHOW GLOBAL STATUS WHERE Variable_name = 'Uptime'", fakeResult{
			columns: []string{"Variable_name", "Value"}, rows: [][]any{{"Uptime", "3600"}},
		}).
		on("performance_schema.setup_consumers", fakeResult{
			columns: []string{"cpu", "global", "thread", "digest", "timed", "mem_on", "mem_all"},
			rows:    [][]any{{"YES", "YES", "YES", "YES", 12, 0, 420}},
		}).
		on("information_schema.USER_PRIVILEGES", fakeResult{
			columns: []string{"PRIVILEGE_TYPE"},
			rows:    [][]any{{"PROCESS"}, {"REPLICATION CLIENT"}, {"performance_schema:SELECT"}},
		}).
		on("information_schema.TABLES", fakeResult{
			columns: []string{"ENGINE", "COUNT(*)"}, rows: [][]any{{"InnoDB", 123}, {"MyISAM", 1}},
		})
}

type fakeProcesses struct {
	processes      []serverProcess
	foreignVisible bool
	container      bool
	unreadable     bool
}

func (f fakeProcesses) serverProcesses() ([]serverProcess, bool, bool) {
	return f.processes, f.foreignVisible, !f.unreadable
}

func (f fakeProcesses) inContainer() bool { return f.container }

func TestMetadataRecordsEveryFlagTheSpecAsksFor(t *testing.T) {
	log := filepath.Join(t.TempDir(), "error.log")
	require.NoError(t, os.WriteFile(log, nil, 0o600))

	m := Collect(context.Background(), metadataServer("8.4.3", log), Target{Host: "db"}, testAt, fakeProcesses{foreignVisible: true})

	assert.Equal(t, "8.4.3", m.Version)
	assert.Equal(t, VariantMySQL, m.MySQLVariant)
	assert.Equal(t, "2026-10-27T09:00:00.123Z", m.ServerUTC)
	assert.Equal(t, "3600", m.UptimeSeconds)
	assert.Equal(t, LogAccessDirect, m.LogAccess)
	assert.Equal(t, "true", m.HasCPUTimeInstrumentEnabled)
	assert.Equal(t, "true", m.HasStatementMemoryTracking)
	assert.Equal(t, "false", m.HasMemoryInstrumentsEnabled, "memory/performance_schema/% is always on and not counted")
	assert.Equal(t, "true", m.HasProcess)
	assert.Equal(t, "true", m.HasReplicationClient)
	assert.Equal(t, "true", m.HasPerformanceSchemaSelect)
	assert.Equal(t, "0.992", m.InnodbTablesShare)
	assert.Equal(t, "MyISAM:1", m.OtherEngines)
	assert.Contains(t, m.SettingsUnavailable, "hostname", "a variable not returned is named, never guessed")
}

func TestMetadataGatesTheStatementColumnsOnVersion(t *testing.T) {
	m := Collect(context.Background(), metadataServer("8.0.27", "stderr"), Target{}, testAt, fakeProcesses{})

	assert.Equal(t, "false", m.HasCPUTimeInstrumentEnabled, "sum_cpu_time is 8.0.28")
	assert.Equal(t, cpuReasonVersion, m.CPUTimeReason)
	assert.Equal(t, "false", m.HasStatementMemoryTracking, "max_total_memory is 8.0.31")
	assert.Equal(t, LogAccessNone, m.LogAccess)
	assert.Equal(t, logReasonStderr, m.LogAccessReason, "the official image's default writes no file")
}

func TestMetadataSaysWhyCPUTimeIsNotCounted(t *testing.T) {
	q := metadataServer("8.0.46", "stderr")
	q.results = append([]fakeMatch{{"performance_schema.setup_consumers", fakeResult{
		columns: []string{"cpu", "global", "thread", "digest", "timed", "mem_on", "mem_all"},
		rows:    [][]any{{"NO", "YES", "YES", "YES", 12, 76, 502}},
	}}}, q.results...)

	m := Collect(context.Background(), q, Target{}, testAt, fakeProcesses{})

	assert.Equal(t, "false", m.HasCPUTimeInstrumentEnabled)
	assert.Equal(t, cpuReasonConsumers, m.CPUTimeReason)
	assert.Equal(t, "true", m.HasMemoryInstrumentsEnabled)
}

func TestMetadataRecordsARefusedProbeAndKeepsTheRest(t *testing.T) {
	q := metadataServer("8.4.3", "stderr")
	q.results = append([]fakeMatch{{"performance_schema.setup_consumers", fakeResult{
		err: serverError(errTableAccess, "SELECT command denied to user 'yc'@'%' for table 'setup_consumers'"),
	}}}, q.results...)

	m := Collect(context.Background(), q, Target{}, testAt, fakeProcesses{})

	assert.Contains(t, m.CapabilitiesError, "SELECT command denied")
	assert.Empty(t, m.HasCPUTimeInstrumentEnabled, "not read is empty, never false")
	assert.Equal(t, "true", m.HasProcess)
}

func TestErrorLogResolvesAsTheServerWould(t *testing.T) {
	tests := []struct{ logError, datadir, want, reason string }{
		{"/var/log/mysql/error.log", "/var/lib/mysql/", "/var/log/mysql/error.log", ""},
		{"./db1.err", "/var/lib/mysql/", "/var/lib/mysql/db1.err", ""},
		{"stderr", "/var/lib/mysql/", "", logReasonStderr},
		{"./db1.err", "", "", logReasonNoDatadir},
	}

	for _, tt := range tests {
		got, reason := resolveErrorLog(tt.logError, tt.datadir)
		assert.Equal(t, tt.want, got, tt.logError)
		assert.Equal(t, tt.reason, reason, tt.logError)
	}
}

func TestSameHostIsTheServersOwnStart(t *testing.T) {
	now := testAt
	started := now.Add(-3600 * time.Second)

	tests := []struct {
		name    string
		procs   fakeProcesses
		verdict string
		reason  string
	}{
		{"a mysqld that started when the server did", fakeProcesses{
			processes: []serverProcess{{pid: 7, started: started.Add(800 * time.Millisecond)}}, foreignVisible: true,
		}, "yes", ""},
		{"another server on this machine", fakeProcesses{
			processes: []serverProcess{{pid: 7, started: started.Add(-time.Hour)}}, foreignVisible: true,
		}, "no", hostReasonStartMismatch},
		{"no server here at all", fakeProcesses{foreignVisible: true}, "no", hostReasonNoServerProcess},
		{"hidden processes prove nothing", fakeProcesses{}, "unknown", hostReasonForeignProcesses},
		{"a container cannot see the host", fakeProcesses{container: true, foreignVisible: true}, "unknown", hostReasonContainer},
		{"no process table", fakeProcesses{unreadable: true}, "unknown", hostReasonProcessesUnread},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Metadata{UptimeSeconds: "3600"}
			collectSameHost(&m, tt.procs, now)

			assert.Equal(t, tt.verdict, m.AgentOnDBHost)
			assert.Equal(t, tt.reason, m.AgentOnDBHostReason)
		})
	}
}

func TestADeclarationRaisesOnlyAnUnknown(t *testing.T) {
	collector := NewMetadata(Target{}, "test", testAt, "")
	collector.DeclareOnDBHost(true)

	m := collector.ResolveHostDecision()
	assert.Equal(t, "yes", m.AgentOnDBHost)
	assert.Equal(t, hostConfirmedByConfigured, m.AgentOnDBHostBy)
	assert.Equal(t, "captured", m.HostArtifacts)

	collector.collected.AgentOnDBHost = "no"
	m = collector.ResolveHostDecision()
	assert.True(t, m.DeclarationContradicted, "a measurement wins")
	assert.Equal(t, "skipped", m.HostArtifacts)
}

func TestTheTargetBlockSurvivesARefusedConnection(t *testing.T) {
	collector := NewMetadata(Target{ID: "orders", Host: "db", Port: 3306, Username: "yc", Password: "s3cret",
		TLS: TLS{Verify: true, CAFile: "/etc/ca.pem"}}, "1.2.3", testAt, "all")

	var out bytes.Buffer

	require.NoError(t, collector.WriteOpening(&out, SampleContext{At: testAt}))

	assert.Contains(t, out.String(), "target_tls_mode,verify\n")
	assert.Contains(t, out.String(), "target_tls_ca_file,/etc/ca.pem\n")
	assert.Contains(t, out.String(), "explain_mode,all\n")
	assert.NotContains(t, out.String(), "s3cret")
}
