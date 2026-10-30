package mysql

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestMetadataSaysWhyTheLogDidNotOpen(t *testing.T) {
	dir := t.TempDir()

	denied := filepath.Join(dir, "error.log")
	require.NoError(t, os.WriteFile(denied, nil, 0o000))

	if os.Geteuid() == 0 {
		t.Skip("root opens a 0000 file")
	}

	for log, want := range map[string]string{
		filepath.Join(dir, "missing.log"): logOpenNotFound,
		denied:                            logOpenPermissionDenied,
	} {
		m := Collect(context.Background(), metadataServer("8.4.3", log), Target{}, testAt, fakeProcesses{})

		assert.Equal(t, LogAccessNone, m.LogAccess, log)
		assert.Equal(t, logReasonUnreadable, m.LogAccessReason, log)
		assert.Equal(t, want, m.LogOpenError, log)
	}
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
		{"a mysqld an entrypoint exec'd: older than the server, its threads are not", fakeProcesses{
			processes: []serverProcess{{
				pid: 1, started: started.Add(-6600 * time.Millisecond), threadsStarted: started.Add(380 * time.Millisecond),
			}}, foreignVisible: true,
		}, "yes", ""},
		{"another server's threads are as old as it is", fakeProcesses{
			processes:      []serverProcess{{pid: 7, started: started.Add(-time.Hour), threadsStarted: started.Add(-time.Hour)}},
			foreignVisible: true,
		}, "no", hostReasonStartMismatch},
		{"a process younger than the server is not it, whatever its threads", fakeProcesses{
			processes:      []serverProcess{{pid: 7, started: started.Add(time.Hour), threadsStarted: started}},
			foreignVisible: true,
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

// statLine is a /proc stat line with starttime, field 22, at ticks.
func statLine(pid int, comm string, ticks int) string {
	// Fields 3 to 21, then starttime and two more.
	return fmt.Sprintf("%d (%s) S %s %d 0 0\n", pid, comm, strings.TrimSpace(strings.Repeat("0 ", 18)), ticks)
}

func TestTheFirstThreadsStartIsReadFromProc(t *testing.T) {
	proc := t.TempDir()

	write := func(path, content string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	}

	// As the matrix's my84-perms showed them: pid 1 an exec'd entrypoint, its
	// first thread 6.62s younger.
	write(filepath.Join(proc, "1", "stat"), statLine(1, "mysqld", 1000))
	write(filepath.Join(proc, "1", "task", "1", "stat"), statLine(1, "mysqld", 1000))
	write(filepath.Join(proc, "1", "task", "214", "stat"), statLine(214, "ib_io_rd-1", 1663))
	write(filepath.Join(proc, "1", "task", "213", "stat"), statLine(213, "ib (io) ibuf", 1662))
	write(filepath.Join(proc, "1", "task", "250", "stat"), statLine(250, "connection", 9000))

	started := testAt.Add(-time.Hour)

	assert.Equal(t, started.Add(6620*time.Millisecond), firstThreadStart(proc, 1, started))
	assert.True(t, firstThreadStart(proc, 2, started).IsZero(), "no such process")

	write(filepath.Join(proc, "3", "stat"), statLine(3, "mysqld", 50))
	write(filepath.Join(proc, "3", "task", "3", "stat"), statLine(3, "mysqld", 50))
	assert.True(t, firstThreadStart(proc, 3, started).IsZero(), "no thread but the first")

	m := Metadata{UptimeSeconds: "3600"}
	collectSameHost(&m, fakeProcesses{
		processes:      []serverProcess{{pid: 1, started: testAt.Add(-3607 * time.Second), threadsStarted: testAt.Add(-3600 * time.Second)}},
		foreignVisible: true,
	}, testAt)

	assert.Equal(t, "yes", m.AgentOnDBHost)
	assert.Equal(t, hostConfirmedByThreadStart, m.AgentOnDBHostBy)
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
