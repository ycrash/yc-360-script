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
		SSLMode:  "require",
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

	reader := csv.NewReader(bytes.NewReader(content))
	reader.FieldsPerRecord = -1

	records, err := reader.ReadAll()
	require.NoError(t, err)
	require.Greater(t, len(records), 2)

	values = map[string]string{}
	for _, record := range records[2:] {
		require.Len(t, record, 2)
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

func TestPostgresMetadataRunUnreachableTarget(t *testing.T) {
	dir := chdirToCaptureDir(t)

	task := &PostgresMetadata{Target: unreachablePostgres(t)}

	result, err := task.Run()
	require.NoError(t, err, "a refused connection is not a capture failure")

	assert.FileExists(t, filepath.Join(dir, PostgresMetadataFileName))

	_, values := readPostgresArtifact(t)
	assert.NotEmpty(t, values["connect_error"], "the file has to say why it is short")
	assert.Equal(t, "unknown", values["capture_mode"])
	assert.Equal(t, "127.0.0.1", values["target_host"])
	assert.NotContains(t, values, "current_database",
		"a non-empty connect_error ends the file")

	assert.Contains(t, result.Msg, PostgresMetadataFileName+" written")
	assert.Contains(t, result.Msg, "postgres connect failed")
}

func TestPostgresMetadataWritesTargetBlockBeforeConnecting(t *testing.T) {
	chdirToCaptureDir(t)

	target := unreachablePostgres(t)
	task := &PostgresMetadata{Target: target}

	_, err := task.Run()
	require.NoError(t, err)

	raw, values := readPostgresArtifact(t)

	assert.True(t, strings.HasPrefix(raw, "# engine=postgres source=pg_metadata "),
		"the block header comes first: %q", raw)
	assert.Equal(t, fmt.Sprint(target.Port), values["target_port"])
	assert.Equal(t, "orders_db", values["target_database"])
	assert.Equal(t, "ycrash_monitor", values["target_username"])
	assert.Equal(t, "require", values["target_sslmode"])
	assert.NotEmpty(t, values["yc360_version"])
}

func TestPostgresMetadataUploadsUnderAssignedDT(t *testing.T) {
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

	task := &PostgresMetadata{Target: unreachablePostgres(t)}
	task.SetEndpoint(server.URL + "?de=test")

	result, err := task.Run()
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, uploads, 1, "the artifact was not uploaded")
	assert.Equal(t, pgDTMetadata, uploads[0].dt, "uploaded under the wrong dt")
	assert.Contains(t, uploads[0].body, "source=pg_metadata",
		"the upload body is not the artifact")
	assert.NotContains(t, uploads[0].body, pgTestPassword, "the upload carries the password")

	assert.Contains(t, result.Msg, PostgresMetadataFileName+" written",
		"the transmission message displaced the capture summary")
}

type recordedPostgresUpload struct {
	dt   string
	body string
}

func TestPostgresDataTypeConstant(t *testing.T) {

	assert.Equal(t, "pgMeta", pgDTMetadata)

	assert.Equal(t, "pgBloat", pgDTBloat)

	taken := []string{
		"meta", "gc", "td", "hd", "ns", "df", "ps", "top", "vmstat", "dmesg",
		"agentlog", "cpuprofile", "kernel", "ping", "hdsub", "lp", "accessLog", "applog",
		nodeDTProcessOverview, nodeDTEventLoopLag, nodeDTUnhandledRejections,
		nodeDTModuleInventory, nodeDTHandleGrowth, nodeDTGCStats,
	}

	for _, dt := range taken {
		assert.NotEqual(t, dt, pgDTMetadata, "pgDTMetadata collides with an existing dt value")
		assert.NotEqual(t, dt, pgDTBloat, "pgDTBloat collides with an existing dt value")
	}

	assert.NotEqual(t, pgDTMetadata, pgDTBloat, "the two postgres artifacts share a dt")
}

func TestPostgresSampledDataTypeGate(t *testing.T) {
	assert.Equal(t, pgDTBloat, pgSampledDataType(postgres.Bloat{}.Artifact()),
		"pg_bloat.txt has its assigned value")

	assert.Empty(t, pgSampledDataType(postgres.Artifact{Name: "pg_health"}),
		"an artifact the server team has not assigned a dt for is not uploaded")
}

func TestPostgresBloatFileNameMatchesTheArtifact(t *testing.T) {
	assert.Equal(t, PostgresBloatFileName, postgres.Bloat{}.Artifact().FileName)
}

func TestPostgresMetadataRedactsPasswordOnConnectFailure(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresMetadata{Target: unreachablePostgres(t)}

	result, err := task.Run()
	require.NoError(t, err)

	raw, _ := readPostgresArtifact(t)
	assert.NotContains(t, raw, pgTestPassword, "the artifact carries the password")
	assert.NotContains(t, result.Msg, pgTestPassword, "the result message carries the password")

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(verb, task), pgTestPassword,
			"the task leaked the password through %s", verb)
	}
}

func TestPostgresMetadataWrapRunFailureCarriesNoPassword(t *testing.T) {
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

	task := &PostgresMetadata{Target: unreachablePostgres(t)}

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

func TestPostgresMetadataNilTarget(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresMetadata{}

	result, err := task.Run()
	require.NoError(t, err)

	assert.Contains(t, result.Msg, "no postgres block configured")
	assert.NoFileExists(t, PostgresMetadataFileName)
}

func pgCollectedMetadata() postgres.Metadata {
	return postgres.Metadata{CaptureMode: "pg-dbhost"}
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
			want:     "pg_metadata.txt written (mode=pg-dbhost)",
		},
		{
			name: "connection refused",
			metadata: postgres.Metadata{
				CaptureMode:  "unknown",
				ConnectError: "failed to connect to `host=127.0.0.1`: connection refused",
			},
			want: "pg_metadata.txt written; postgres connect failed: " +
				"failed to connect to `host=127.0.0.1`: connection refused",
		},
		{

			name: "database at max_connections",
			metadata: postgres.Metadata{
				CaptureMode:  "unknown",
				ConnectError: "too_many_connections",
			},
			want: "pg_metadata.txt written; postgres connect failed: too_many_connections",
		},
		{

			name: "a probe was denied",
			metadata: postgres.Metadata{
				CaptureMode:         "unknown",
				CurrentLogfileError: "ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
			},
			want: "pg_metadata.txt written (mode=unknown); pg_current_logfile failed: " +
				"ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
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

	content, err := os.ReadFile(PostgresBloatFileName)
	require.NoError(t, err)

	return string(content)
}

func TestPostgresSamplerRunUnreachableTarget(t *testing.T) {
	dir := chdirToCaptureDir(t)

	task := &PostgresSampler{Target: withWindow(t, 2*time.Minute)}

	started := time.Now()
	result, err := task.Run()
	elapsed := time.Since(started)

	require.NoError(t, err, "a refused connection is not a capture failure")
	assert.FileExists(t, filepath.Join(dir, PostgresBloatFileName))

	artifact := readBloatArtifact(t)
	assert.Contains(t, artifact, "status=started", "the preamble is written before connecting")
	assert.Contains(t, artifact, "status=connect_failed")
	assert.Contains(t, artifact, "samples_written=0")

	assert.Less(t, elapsed, 30*time.Second,
		"a connect failure waited out the window instead of failing fast")

	assert.Contains(t, result.Msg, PostgresBloatFileName+" written (0/2 samples)")
	assert.Contains(t, result.Msg, "postgres connect failed")
}

func TestPostgresSamplerUploadsUnderAssignedDT(t *testing.T) {
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

	task := &PostgresSampler{Target: withWindow(t, time.Second)}
	task.SetEndpoint(server.URL + "?de=test")

	result, err := task.Run()
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, uploads, 1, "the artifact was not uploaded")
	assert.Equal(t, pgDTBloat, uploads[0].dt, "uploaded under the wrong dt")
	assert.Contains(t, uploads[0].body, "source=pg_bloat", "the upload body is not the artifact")
	assert.Contains(t, uploads[0].body, "status=connect_failed",
		"a capture of a failure is uploaded like any other")
	assert.NotContains(t, uploads[0].body, pgTestPassword, "the upload carries the password")

	assert.Contains(t, result.Msg, PostgresBloatFileName+" written",
		"the transmission message displaced the capture summary")
}

func TestPostgresSamplerRedactsPasswordOnConnectFailure(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresSampler{Target: withWindow(t, time.Second)}

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

func TestPostgresSamplerWrapRunFailureCarriesNoPassword(t *testing.T) {
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

	task := &PostgresSampler{Target: withWindow(t, time.Second)}

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

func TestPostgresSamplerNilTarget(t *testing.T) {
	chdirToCaptureDir(t)

	result, err := (&PostgresSampler{}).Run()
	require.NoError(t, err)

	assert.Contains(t, result.Msg, "no postgres block configured")
	assert.NoFileExists(t, PostgresBloatFileName)
}

func TestPostgresSamplerKillCancelsAnInFlightWindow(t *testing.T) {
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
	target.SSLMode = "disable"

	task := &PostgresSampler{Target: target}

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

func TestPostgresSamplerKillOutsideARun(t *testing.T) {
	task := &PostgresSampler{Target: unreachablePostgres(t)}

	assert.NoError(t, task.Kill(), "Kill before Run must not panic")

	chdirToCaptureDir(t)
	_, err := task.Run()
	require.NoError(t, err)

	assert.NoError(t, task.Kill(), "Kill after Run must not panic")
}

func TestPostgresSamplerDefaultsTheWindowWhenUnvalidated(t *testing.T) {
	task := &PostgresSampler{Target: unreachablePostgres(t)}
	require.Nil(t, task.Target.CaptureDuration)

	assert.Equal(t, config.DefaultPostgresCaptureDuration, task.captureDuration())

	window := config.Duration(45 * time.Second)
	task.Target.CaptureDuration = &window
	assert.Equal(t, 45*time.Second, task.captureDuration())
}

func TestPostgresSamplerMessage(t *testing.T) {
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, postgresSamplerMessage(tt.result))
		})
	}
}
