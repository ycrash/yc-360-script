package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errDenied = errors.New("ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)")

type fakeMetadataConn struct {
	*fakeWindowConn

	querier *fakeQuerier
}

func newFakeMetadataConn() *fakeMetadataConn {
	return &fakeMetadataConn{fakeWindowConn: newFakeWindowConn(), querier: healthyQuerier()}
}

// testConnectDuration must render as fullArtifactMetadata's connect_ms: the golden
// goes through the window, the comparison through a hand-built Metadata.
const testConnectDuration = 12400 * time.Microsecond

func (c *fakeMetadataConn) ConnectDuration() time.Duration { return testConnectDuration }

func (c *fakeMetadataConn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == currentDatabaseSQL {
		return c.fakeWindowConn.QueryRow(ctx, sql, args...)
	}

	return c.querier.QueryRow(ctx, sql, args...)
}

func metadataAt(minute, second, milli int) time.Time {
	return time.Date(2026, 8, 4, 9, minute, second, milli*int(time.Millisecond), time.UTC)
}

func runMetadataWindow(t *testing.T, clock *scriptedClock, collector *MetadataCollector,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   120 * time.Second,
		Collectors: []Collector{collector},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func collecting(m Metadata) *MetadataCollector {
	// The mode is supplied at construction, not read; passing the fixture's keeps the
	// two from disagreeing.
	collector := NewMetadata(testTarget(), "3.6.1", m.AgentTS, m.ExplainMode)
	collector.collect = func(context.Context, Querier, Target, time.Time) Metadata { return m }

	return collector
}

func TestMetadataArtifact(t *testing.T) {
	artifact := NewMetadata(testTarget(), "3.6.1", testAgentNow, "").Artifact()

	assert.Equal(t, "pg_metadata", artifact.Name)
	assert.Equal(t, "pg_metadata.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"the capability read describes the server, not the connected database")

	assert.Equal(t, Once(), artifact.Schedule,
		"a capability read is one reading, taken as the window opens")
	assert.Equal(t, []time.Duration{0}, artifact.Schedule.offsets(120*time.Second))
}

func TestMetadataGoldenFull(t *testing.T) {
	clock := newScriptedClock(t,
		metadataAt(12, 44, 118),
		metadataAt(12, 44, 119),
		metadataAt(12, 44, 120),
		metadataAt(12, 44, 120),
		metadataAt(12, 44, 201),
		metadataAt(14, 44, 125),
	)

	collector := collecting(fullArtifactMetadata())

	results := runMetadataWindow(t, clock, collector, connectTo(newFakeMetadataConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 1, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_metadata_full.txt"), artifactText(t, results[0]))
}

func TestMetadataGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t,
		metadataAt(12, 49, 201),
		metadataAt(12, 49, 202),
		metadataAt(12, 54, 215),
	)

	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow, ExplainModeAll)

	results := runMetadataWindow(t, clock, collector,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_metadata_connect_failure.txt"), artifactText(t, results[0]))
}

func TestMetadataConnectFailureWritesNoServerBlock(t *testing.T) {
	clock := newScriptedClock(t,
		metadataAt(12, 49, 201),
		metadataAt(12, 49, 202),
		metadataAt(12, 54, 215),
	)

	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow, "")

	results := runMetadataWindow(t, clock, collector,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "source=pg_metadata_server",
		"the server block exists only because a connection did")

	_, values, _ := parseArtifact(t, artifact)
	assert.NotContains(t, values, "current_database")
	assert.NotContains(t, values, "log_access",
		"with no connection the mode is unknown by construction, and the closing block "+
			"says connect_failed about the capture rather than about the server")
	assert.Equal(t, "orders_db", values["target_database"],
		"the block a reader can rely on is the one that is always there")

	headers := headersOf(t, results[0])
	require.Len(t, headers, 3)
	assert.Contains(t, headers[2], "status=connect_failed samples_expected=1 samples_written=0 "+
		"connect_error=too_many_connections")
}

func TestMetadataArtifactParsesByTheDocumentedRule(t *testing.T) {
	clock := newScriptedClock(t,
		metadataAt(12, 49, 201),
		metadataAt(12, 49, 202),
		metadataAt(12, 54, 215),
	)

	refused := errors.New("failed to connect to `user=ycrash_monitor database=orders_db`: " +
		"hostname resolving error: lookup db-prod-01.internal: no such host")

	results := runMetadataWindow(t, clock, NewMetadata(testTarget(), "3.6.1", testConnectFailureNow, ""),
		func(context.Context, Target) (windowConn, error) { return nil, refused })

	artifact := artifactText(t, results[0])

	headers, values, _ := parseArtifact(t, artifact)

	require.Len(t, headers, 3, "preamble, target block, closing block")
	assert.Contains(t, headers[2], "connect_error=")
	assert.Equal(t, "orders_db", values["target_database"],
		"the body parses whatever the headers carry")

	reader := csv.NewReader(strings.NewReader(artifact))
	reader.FieldsPerRecord = -1

	_, err := reader.ReadAll()
	require.Error(t, err,
		"a reader that CSV-parses the whole file breaks on an ordinary refused connection")
}

func TestMetadataBlocksCarryTheirOwnKeys(t *testing.T) {
	clock := newScriptedClock(t,
		metadataAt(12, 44, 118),
		metadataAt(12, 44, 119),
		metadataAt(12, 44, 120),
		metadataAt(12, 44, 120),
		metadataAt(12, 44, 201),
		metadataAt(14, 44, 125),
	)

	full := fullArtifactMetadata()

	results := runMetadataWindow(t, clock, collecting(full), connectTo(newFakeMetadataConn()))

	headers, values, keys := parseArtifact(t, artifactText(t, results[0]))

	require.Len(t, headers, 4, "preamble, target block, server block, closing block")
	assert.Contains(t, headers[1], "source=pg_metadata_target")
	assert.Contains(t, headers[1], "db=orders_db dbid= ts=",
		"there is no connection yet, so the configured name and no OID")
	assert.Contains(t, headers[2], "source=pg_metadata_server")
	assert.Contains(t, headers[2], "db=orders_db dbid=16401 sample=1 ts=",
		"and afterwards, what identify read")

	var want []string
	for _, f := range append(targetFields(full), serverBlockFields(full)...) {
		want = append(want, f.key)
	}

	assert.Equal(t, want, keys)
	assert.Len(t, serverBlockFields(full), 67,
		"log_access and log_access_reason plus serverFields' sixty-five, and no connect_error row")
	assert.Len(t, targetFields(full), 9)

	assert.Equal(t, LogAccessDirect, values["log_access"])
	assert.Equal(t, "3.6.1", values["yc360_version"])
}

func TestMetadataWritesEachBlockInOneWrite(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	opening := &countingWriter{}
	require.NoError(t, collector.WriteOpening(opening, SampleContext{At: testAgentNow}))
	assert.Equal(t, 1, opening.writes)
	assert.NotEmpty(t, opening.buf.String())

	sample := &countingWriter{}
	require.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), sample,
		SampleContext{At: testAgentNow, Index: 1, Database: "orders_db", DBID: "16401"}))

	assert.Equal(t, 1, sample.writes,
		"a write failing between header and body would leave the window's stub "+
			"behind a half-written block")
	assert.NotEmpty(t, sample.buf.String())
}

func TestMetadataCollectedIsWhatCollectReturned(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	assert.Equal(t, LogAccessUnknown, collector.Collected().LogAccess,
		"before the sample the mode is unknown, which is also the truth about a "+
			"run whose connection was refused")
	assert.Equal(t, "orders_db", collector.Collected().TargetDatabase)

	conn := newFakeMetadataConn()
	conn.querier.logLocation = fakeRow{err: errDenied}

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf,
		SampleContext{At: testAgentNow, Index: 1, Database: "orders_db", DBID: "16401"}))

	collected := collector.Collected()

	assert.Equal(t, "orders_db", collected.CurrentDatabase)
	assert.Contains(t, collected.CurrentLogfileError, "permission denied for function pg_current_logfile",
		"the probe error the run log's line for this artifact is rendered from")
	assert.Equal(t, "3.6.1", collected.YC360Version,
		"Collect returns a fresh value, so the version stamped before the connection is carried across")
}

func TestMetadataCollectorCarriesNoPasswordThroughFmt(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(verb, collector), testPassword,
			"the collector leaked the password through %s", verb)
	}

	assert.Contains(t, fmt.Sprintf("%v", collector), "<redacted>")

	assert.NotContains(t, fmt.Sprintf("%#v", collector.Collected()), testPassword,
		"and what the adapter reads back has no password field to leak")
}

func TestMetadataSampleCarriesNoPassword(t *testing.T) {
	conn := newFakeMetadataConn()
	leak := errors.New("connect failed for password=" + testPassword)
	conn.querier.serverFacts = fakeRow{err: leak}
	conn.querier.logLocation = fakeRow{err: leak}
	conn.querier.replication = fakeRow{err: leak}

	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf,
		SampleContext{At: testAgentNow, Index: 1, Database: "orders_db", DBID: "16401"}))

	assert.NotContains(t, buf.String(), testPassword)
	assert.Contains(t, buf.String(), "<redacted>")
}

func TestCollect(t *testing.T) {
	m := collect(t, healthyQuerier())

	assert.Equal(t, "db-prod-01.internal", m.TargetHost)
	assert.Equal(t, 5432, m.TargetPort)
	assert.Equal(t, "orders_db", m.TargetDatabase)
	assert.Equal(t, "ycrash_monitor", m.TargetUsername)
	assert.Equal(t, "require", m.TargetSSLMode)
	assert.Equal(t, testAgentNow, m.AgentTS)

	assert.Equal(t, "orders_db", m.CurrentDatabase)
	assert.Equal(t, "ycrash_monitor", m.CurrentUser)
	assert.Equal(t, "48211", m.BackendPID)
	assert.Equal(t, "10.0.4.7", m.InetServerAddr)
	assert.Equal(t, "5432", m.InetServerPort)
	assert.Equal(t, "false", m.IsInRecovery)
	assert.Equal(t, "PostgreSQL 15.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0", m.Version)
	assert.Equal(t, "150004", m.ServerVersionNum)
	assert.Equal(t, "true", m.ReplicationConfigured)

	assert.Empty(t, m.QueryError)
	assert.Empty(t, m.CurrentLogfileError)
	assert.Empty(t, m.ReplicationProbeError)
	assert.Empty(t, m.ConnectError, "Collect is only reached once a connection exists")
}

func TestCollectServerFactsFailure(t *testing.T) {
	q := healthyQuerier()
	q.serverFacts = fakeRow{err: errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")}

	m := collect(t, q)

	assert.Contains(t, m.QueryError, "statement timeout")

	assert.Equal(t, "db-prod-01.internal", m.TargetHost)
	assert.Equal(t, "orders_db", m.TargetDatabase)

	assert.Empty(t, m.CurrentDatabase)
	assert.Empty(t, m.ServerVersionNum)
	assert.Empty(t, m.MaxConnections)
	assert.Empty(t, m.HasPgStatCheckpointer)
	assert.Empty(t, m.ServerNow)

	assert.Empty(t, m.SettingsUnavailable)

	assert.Equal(t, "log/postgresql-2026-08-04_000000.csv", m.CurrentLogfile)
	assert.Empty(t, m.DataDirectory)
	assert.Empty(t, m.CurrentLogfileResolved)
	assert.Equal(t, LogAccessUnknown, m.LogAccess,
		"every route reads the settings this statement collects, so a failure here is "+
			"detection that could not run rather than detection that found nothing")
	assert.Equal(t, "true", m.ReplicationConfigured)
}

func TestCollectLogLocationDeniedFallsThroughToTheDisk(t *testing.T) {
	dir := t.TempDir()

	logfile := filepath.Join(dir, "postgresql-2026-08-04_000000.log")
	require.NoError(t, os.WriteFile(logfile, []byte("LOG: ready\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "current_logfiles"),
		[]byte("stderr postgresql-2026-08-04_000000.log\n"), 0o600))

	q := healthyQuerier()
	q.logLocation = fakeRow{err: errDenied}

	settings := fullSettings()
	settings["data_directory"] = dir
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(settings)

	m := collect(t, q)

	assert.Equal(t, LogAccessDirect, m.LogAccess, "the 14 to 16 case, and the reason for the slice")
	assert.Equal(t, resolvedByCurrentLogfiles, m.LogResolvedBy)
	assert.Equal(t, "stderr", m.LogFormats)

	assert.Equal(t, logfile, m.CurrentLogfileResolved)
	assert.Empty(t, m.LogAccessReason, "direct access leaves the reason empty")
	assert.Empty(t, m.CurrentLogfileError, "no route had to report one")

	assert.Equal(t, "orders_db", m.CurrentDatabase)
	assert.Empty(t, m.QueryError)
}

func TestCollectRecordsTheLastRoutesError(t *testing.T) {
	q := healthyQuerier()
	q.logLocation = fakeRow{err: errDenied}

	m := collect(t, q)

	assert.Contains(t, m.CurrentLogfileError, "permission denied for function pg_current_logfile")
	assert.Equal(t, LogAccessNone, m.LogAccess, "no route produced a path")
	assert.Empty(t, m.LogResolvedBy)

	assert.Equal(t, "/var/lib/postgresql/15/main", m.DataDirectory)
	assert.Empty(t, m.QueryError)
}

func TestCollectReplicationDenied(t *testing.T) {
	q := healthyQuerier()
	q.replication = fakeRow{err: errors.New("ERROR: permission denied for view pg_stat_replication (SQLSTATE 42501)")}

	m := collect(t, q)

	assert.Contains(t, m.ReplicationProbeError, "permission denied for view pg_stat_replication")
	assert.Empty(t, m.ReplicationConfigured)

	assert.Equal(t, "orders_db", m.CurrentDatabase)
	assert.NotEqual(t, LogAccessUnknown, m.LogAccess)
	assert.Empty(t, m.QueryError)
}

func TestReplicationConfigured(t *testing.T) {
	for _, tt := range []struct {
		name  string
		count *int64
		want  string
	}{
		{name: "a standby is streaming", count: ptr(int64(2)), want: "true"},
		{name: "nothing is streaming", count: ptr(int64(0)), want: "false"},
		{name: "no row", count: nil, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			q := healthyQuerier()
			q.replication = fakeRow{values: []any{tt.count}}

			assert.Equal(t, tt.want, collect(t, q).ReplicationConfigured)
		})
	}
}

func TestLogAccess(t *testing.T) {
	tests := []struct {
		name string

		setup            func(t *testing.T, dir string) (logfile any, dataDirectory, resolved string)
		loggingCollector string
		logLocationErr   error
		wantAccess       string
		wantReason       string
	}{
		{
			name: "collector on, but no route names a file",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return nil, dir, ""
			},
			wantAccess: LogAccessNone,
			wantReason: reasonUnresolved,
		},
		{
			name: "logging_collector off",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return nil, dir, ""
			},
			loggingCollector: "off",
			wantAccess:       LogAccessNone,
			wantReason:       reasonCollectorOff,
		},
		{
			name: "absolute path, readable",
			setup: func(t *testing.T, dir string) (any, string, string) {
				path := filepath.Join(dir, "postgresql-2026-08-04_000000.log")
				require.NoError(t, os.WriteFile(path, []byte("LOG: ready\n"), 0o600))

				return ptr(path), dir, path
			},
			wantAccess: LogAccessDirect,
			wantReason: "",
		},
		{
			name: "relative path, resolved against data_directory",
			setup: func(t *testing.T, dir string) (any, string, string) {
				require.NoError(t, os.Mkdir(filepath.Join(dir, "log"), 0o700))

				relative := filepath.Join("log", "postgresql-2026-08-04_000000.csv")
				path := filepath.Join(dir, relative)
				require.NoError(t, os.WriteFile(path, []byte("2026-08-04,LOG\n"), 0o600))

				return ptr(relative), dir, path
			},
			wantAccess: LogAccessDirect,
			wantReason: "",
		},
		{
			name: "path does not exist here",
			setup: func(t *testing.T, dir string) (any, string, string) {
				path := filepath.Join(dir, "not-this-host.log")

				return ptr(path), dir, path
			},
			wantAccess: LogAccessNone,
			wantReason: reasonUnreadable,
		},
		{

			name: "path exists but is not readable",
			setup: func(t *testing.T, dir string) (any, string, string) {
				requireUnprivileged(t)

				path := filepath.Join(dir, "postgresql-2026-08-04_000000.log")
				require.NoError(t, os.WriteFile(path, []byte("LOG: ready\n"), 0o000))

				return ptr(path), dir, path
			},
			wantAccess: LogAccessNone,
			wantReason: reasonUnreadable,
		},
		{

			name: "relative path with no data_directory to resolve against",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return ptr("log/postgresql-2026-08-04_000000.csv"), "", ""
			},
			wantAccess: LogAccessNone,
			wantReason: reasonUnresolved,
		},
		{
			name: "log location denied and no route resolves",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return nil, dir, ""
			},
			logLocationErr: errDenied,
			wantAccess:     LogAccessNone,
			wantReason:     reasonUnresolved,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			logfile, dataDirectory, resolved := tt.setup(t, dir)

			q := healthyQuerier()

			settings := fullSettings()
			if dataDirectory == "" {
				delete(settings, "data_directory")
			} else {
				settings["data_directory"] = dataDirectory
			}

			if tt.loggingCollector != "" {
				settings["logging_collector"] = tt.loggingCollector
			}
			q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(settings)

			if tt.logLocationErr != nil {
				q.logLocation = fakeRow{err: tt.logLocationErr}
			} else {
				q.logLocation = fakeRow{values: logLocationValues(logfile)}
			}

			m := collect(t, q)

			assert.Equal(t, tt.wantAccess, m.LogAccess)
			assert.Equal(t, tt.wantReason, m.LogAccessReason,
				"a reason is mandatory whenever access is not direct")

			assert.Equal(t, resolved, m.CurrentLogfileResolved)
		})
	}
}

func TestLogAccessResolvesByGlobWhereNeitherTheFileNorTheFunctionIsReachable(t *testing.T) {
	logDirectory := t.TempDir()

	older := filepath.Join(logDirectory, "postgresql-2026-08-04_000000.csv")
	newest := filepath.Join(logDirectory, "postgresql-2026-08-04_120000.csv")
	require.NoError(t, os.WriteFile(older, []byte("old\n"), 0o600))
	require.NoError(t, os.WriteFile(newest, []byte("new\n"), 0o600))

	stale := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(older, stale, stale))

	q := healthyQuerier()
	q.logLocation = fakeRow{err: errDenied}

	settings := fullSettings()
	settings["data_directory"] = filepath.Join(logDirectory, "unreadable")
	settings["log_directory"] = logDirectory
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(settings)

	m := collect(t, q)

	assert.Equal(t, LogAccessDirect, m.LogAccess)
	assert.Equal(t, resolvedByGlob, m.LogResolvedBy)
	assert.Equal(t, newest, m.CurrentLogfileResolved,
		"the newest match, and the .log suffix replaced with .csv because log_filename names "+
			"only the stderr file")
	assert.Empty(t, m.LogAccessReason, "direct access leaves the reason empty")
}

func TestLogAccessAtThePrivilegeFloorHasNoRouteAtAll(t *testing.T) {
	q := healthyQuerier()
	q.logLocation = fakeRow{err: errDenied}

	settings := fullSettings()
	for _, name := range []string{"data_directory", "log_directory", "log_filename"} {
		delete(settings, name)
	}
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(settings)

	m := collect(t, q)

	assert.Equal(t, LogAccessNone, m.LogAccess)
	assert.Empty(t, m.LogResolvedBy)
	assert.Empty(t, m.CurrentLogfileResolved)
	assert.Contains(t, m.CurrentLogfileError, "permission denied for function pg_current_logfile")
}

func TestCollectCarriesNoPassword(t *testing.T) {
	leak := errors.New("connect failed for password=" + testPassword)

	q := &fakeQuerier{
		serverFacts: fakeRow{err: leak},
		logLocation: fakeRow{err: leak},
		replication: fakeRow{err: leak},
	}

	m := collect(t, q)

	require.NotEmpty(t, m.QueryError)
	require.NotEmpty(t, m.CurrentLogfileError)
	require.NotEmpty(t, m.ReplicationProbeError)

	for name, value := range stringFields(m) {
		assert.NotContains(t, value, testPassword, "Metadata.%s carries the password", name)
	}
}

func TestMetadataHasNoPasswordField(t *testing.T) {
	for _, field := range reflect.VisibleFields(reflect.TypeFor[Metadata]()) {
		assert.NotContains(t, field.Name, "Password")
	}
}

func stringFields(m Metadata) map[string]string {
	v := reflect.ValueOf(m)

	out := map[string]string{}
	for _, field := range reflect.VisibleFields(v.Type()) {
		if field.Type.Kind() == reflect.String {
			out[field.Name] = v.FieldByIndex(field.Index).String()
		}
	}

	return out
}

func requireUnprivileged(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("file mode permissions do not apply on windows")
	}

	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 is still readable")
	}
}

func TestMetadataConnectFailureLeavesHostCaptureOff(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow, "")

	collected := collector.ResolveHostDecision()

	assert.Equal(t, OnDBHostUnknown, collected.AgentOnDBHost,
		"there was no backend to look for, so the probe never ran")
	assert.Equal(t, hostReasonNoConnection, collected.AgentOnDBHostReason)
	assert.Equal(t, HostArtifactsSkipped, collected.HostArtifacts)
	assert.False(t, collected.DeclarationContradicted)
}

func TestMetadataDeclarationAuthorisesHostCaptureOnAConnectFailure(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow, "")
	collector.DeclareOnDBHost(true)

	collected := collector.ResolveHostDecision()

	assert.Equal(t, OnDBHostYes, collected.AgentOnDBHost)
	assert.Equal(t, confirmedByConfigured, collected.AgentOnDBHostBy,
		"recorded as a claim, so a reader can tell it from a measurement")
	assert.Equal(t, HostArtifactsCaptured, collected.HostArtifacts)
}

func TestMetadataSampleRunsAfterCollectWithTheResolvedVerdict(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")
	collector.DeclareOnDBHost(true)

	var seen []Metadata
	collector.AfterCollect(func(m Metadata) { seen = append(seen, m) })

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), &buf,
		SampleContext{At: testAgentNow, Index: 1, Database: "orders_db", DBID: "16401"}))

	require.Len(t, seen, 1, "the sample happens once, so the callback runs once")
	assert.Equal(t, collector.Collected(), seen[0])
	assert.NotEmpty(t, seen[0].HostArtifacts, "the gate's decision is settled before the callback runs")
	assert.Contains(t, buf.String(), "host_artifacts,"+seen[0].HostArtifacts,
		"the block carries the same decision the callback acted on")
}

func TestMetadataAfterCollectIsOptional(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	var buf bytes.Buffer
	assert.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), &buf,
		SampleContext{At: testAgentNow, Index: 1}))
}

// slowQuerier delays the server-facts query so the bracket has something to measure.
type slowQuerier struct {
	*fakeQuerier

	delay time.Duration
}

func (q slowQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if sql == serverFactsSQL {
		time.Sleep(q.delay)
	}

	return q.fakeQuerier.QueryRow(ctx, sql, args...)
}

func TestClockReadIsStampedAtTheQuery(t *testing.T) {
	const delay = 40 * time.Millisecond

	before := time.Now()
	m := Collect(context.Background(), slowQuerier{fakeQuerier: healthyQuerier(), delay: delay},
		testTarget(), testAgentNow)

	require.Empty(t, m.QueryError)

	assert.True(t, m.AgentTSAtClockRead.After(before.Add(delay)),
		"stamped when the server's clock came back, not when the collector was built")

	rtt, err := strconv.ParseFloat(m.ClockReadRTTMS, 64)
	require.NoError(t, err, "clock_read_rtt_ms must parse as a number")

	assert.GreaterOrEqual(t, rtt, float64(delay.Milliseconds()),
		"the round trip bounds how much the skew figure beside it is worth")
}

func TestClockReadIsEmptyWhenTheQueryFailed(t *testing.T) {
	q := healthyQuerier()
	q.serverFacts = fakeRow{err: errDenied}

	m := Collect(context.Background(), q, testTarget(), testAgentNow)

	require.NotEmpty(t, m.QueryError)

	assert.True(t, m.AgentTSAtClockRead.IsZero(),
		"there was no server clock to read the agent's beside")
	assert.Empty(t, m.ClockReadRTTMS)
	assert.Empty(t, clockRead(m.AgentTSAtClockRead),
		"and the row is empty rather than year one, which would read as a two-thousand-year skew")
}

func TestConnectMSComesFromTheConnection(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), &buf,
		SampleContext{At: testAgentNow, Index: 1, ConnectDuration: testConnectDuration}))

	assert.Equal(t, "12.4", collector.Collected().ConnectMS)
	assert.Contains(t, buf.String(), "connect_ms,12.4")
}

func TestConnectMSIsEmptyWhenThereWasNoDial(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), &buf,
		SampleContext{At: testAgentNow, Index: 1}))

	assert.Empty(t, collector.Collected().ConnectMS,
		"an unmeasured duration is empty, never zero - the file's rule for every unread value")
}

func TestMillisText(t *testing.T) {
	for _, tt := range []struct {
		in   time.Duration
		want string
	}{
		{0, ""},
		{-time.Second, ""},
		{12400 * time.Microsecond, "12.4"},
		{1500 * time.Millisecond, "1500.0"},
		{40 * time.Microsecond, "0.0"},
	} {
		assert.Equal(t, tt.want, millisText(tt.in), tt.in.String())
	}
}
