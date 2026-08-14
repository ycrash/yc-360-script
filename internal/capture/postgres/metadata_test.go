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
	collector := NewMetadata(testTarget(), "3.6.1", m.AgentTS)
	collector.collect = func(context.Context, Querier, Target, time.Time) Metadata { return m }

	return collector
}

func TestMetadataArtifact(t *testing.T) {
	artifact := NewMetadata(testTarget(), "3.6.1", testAgentNow).Artifact()

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

	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow)

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

	collector := NewMetadata(testTarget(), "3.6.1", testConnectFailureNow)

	results := runMetadataWindow(t, clock, collector,
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "source=pg_metadata_server",
		"the server block exists only because a connection did")

	_, values, _ := parseArtifact(t, artifact)
	assert.NotContains(t, values, "current_database")
	assert.NotContains(t, values, "capture_mode",
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

	results := runMetadataWindow(t, clock, NewMetadata(testTarget(), "3.6.1", testConnectFailureNow),
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
	assert.Len(t, serverBlockFields(full), 45,
		"capture_mode plus serverFields' forty-four, and no connect_error row")
	assert.Len(t, targetFields(full), 7)

	assert.Equal(t, ModeDBHost, values["capture_mode"])
	assert.Equal(t, "3.6.1", values["yc360_version"])
}

func TestMetadataWritesEachBlockInOneWrite(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow)

	prologue := &countingWriter{}
	require.NoError(t, collector.WritePrologue(prologue, SampleContext{At: testAgentNow}))
	assert.Equal(t, 1, prologue.writes)
	assert.NotEmpty(t, prologue.buf.String())

	sample := &countingWriter{}
	require.NoError(t, collector.Sample(context.Background(), newFakeMetadataConn(), sample,
		SampleContext{At: testAgentNow, Index: 1, Database: "orders_db", DBID: "16401"}))

	assert.Equal(t, 1, sample.writes,
		"a write failing between header and body would leave the window's stub "+
			"behind a half-written block")
	assert.NotEmpty(t, sample.buf.String())
}

func TestMetadataCollectedIsWhatCollectReturned(t *testing.T) {
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow)

	assert.Equal(t, ModeUnknown, collector.Collected().CaptureMode,
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
	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow)

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

	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow)

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
	assert.Equal(t, ModeRemote, m.CaptureMode)
	assert.Equal(t, "true", m.ReplicationConfigured)
}

func TestCollectLogLocationDenied(t *testing.T) {
	q := healthyQuerier()
	q.logLocation = fakeRow{err: errDenied}

	m := collect(t, q)

	assert.Contains(t, m.CurrentLogfileError, "permission denied for function pg_current_logfile")
	assert.Equal(t, ModeUnknown, m.CaptureMode, "a denial says nothing about where the agent runs")

	assert.Empty(t, m.CurrentLogfile)
	assert.Empty(t, m.CurrentLogfileResolved)
	assert.Empty(t, m.CurrentLogfileReadable)

	assert.Equal(t, "/var/lib/postgresql/15/main", m.DataDirectory)

	assert.Equal(t, "orders_db", m.CurrentDatabase)
	assert.Equal(t, "150004", m.ServerVersionNum)
	assert.Equal(t, "200", m.MaxConnections)
	assert.Empty(t, m.QueryError)
}

func TestCollectReplicationDenied(t *testing.T) {
	q := healthyQuerier()
	q.replication = fakeRow{err: errors.New("ERROR: permission denied for view pg_stat_replication (SQLSTATE 42501)")}

	m := collect(t, q)

	assert.Contains(t, m.ReplicationProbeError, "permission denied for view pg_stat_replication")
	assert.Empty(t, m.ReplicationConfigured)

	assert.Equal(t, "orders_db", m.CurrentDatabase)
	assert.NotEqual(t, ModeUnknown, m.CaptureMode)
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

func TestCaptureMode(t *testing.T) {
	tests := []struct {
		name string

		setup          func(t *testing.T, dir string) (logfile any, dataDirectory, resolved string)
		logLocationErr error
		wantMode       string
		wantReadable   string
	}{
		{
			name: "logging_collector off",
			setup: func(t *testing.T, dir string) (any, string, string) {

				return nil, dir, ""
			},
			wantMode:     ModeRemote,
			wantReadable: "",
		},
		{
			name: "absolute path, readable",
			setup: func(t *testing.T, dir string) (any, string, string) {
				path := filepath.Join(dir, "postgresql-2026-08-04_000000.log")
				require.NoError(t, os.WriteFile(path, []byte("LOG: ready\n"), 0o600))

				return ptr(path), dir, path
			},
			wantMode:     ModeDBHost,
			wantReadable: "true",
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
			wantMode:     ModeDBHost,
			wantReadable: "true",
		},
		{
			name: "path does not exist here",
			setup: func(t *testing.T, dir string) (any, string, string) {
				path := filepath.Join(dir, "not-this-host.log")

				return ptr(path), dir, path
			},
			wantMode:     ModeRemote,
			wantReadable: "false",
		},
		{

			name: "path exists but is not readable",
			setup: func(t *testing.T, dir string) (any, string, string) {
				requireUnprivileged(t)

				path := filepath.Join(dir, "postgresql-2026-08-04_000000.log")
				require.NoError(t, os.WriteFile(path, []byte("LOG: ready\n"), 0o000))

				return ptr(path), dir, path
			},
			wantMode:     ModeRemote,
			wantReadable: "false",
		},
		{

			name: "relative path with no data_directory to resolve against",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return ptr("log/postgresql-2026-08-04_000000.csv"), "", ""
			},
			wantMode:     ModeRemote,
			wantReadable: "",
		},
		{
			name: "log location denied",
			setup: func(t *testing.T, dir string) (any, string, string) {
				return nil, dir, ""
			},
			logLocationErr: errDenied,
			wantMode:       ModeUnknown,
			wantReadable:   "",
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
			q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(settings)

			if tt.logLocationErr != nil {
				q.logLocation = fakeRow{err: tt.logLocationErr}
			} else {
				q.logLocation = fakeRow{values: logLocationValues(logfile)}
			}

			m := collect(t, q)

			assert.Equal(t, tt.wantMode, m.CaptureMode)
			assert.Equal(t, tt.wantReadable, m.CurrentLogfileReadable)

			assert.Equal(t, resolved, m.CurrentLogfileResolved)
		})
	}
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
