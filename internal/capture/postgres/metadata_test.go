package postgres

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errDenied = errors.New("ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)")

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
