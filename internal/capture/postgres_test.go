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

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// Deliberately distinctive: the leak assertions grep for this exact string.
const pgTestPassword = "s3cr3t-do-not-log"

// Targets a loopback port nothing is listening on, so the connection is refused
// immediately rather than waiting out a DNS or TCP timeout.
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

// Mimics what FullCapture has already done by the time a capture runs: the
// working directory is the capture directory.
func chdirToCaptureDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	previous, err := os.Getwd()
	require.NoError(t, err)

	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(previous) })

	// t.TempDir hands back a path that can be a symlink (/var -> /private/var on
	// macOS); resolving it keeps the assertions about where the file landed
	// comparable.
	resolved, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)

	return resolved
}

// Reads pg_metadata.txt the way the artifact's contract says a reader should:
// CSV records, not lines, with a variable field count for the block header.
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

// Redirects the agent log for the duration of one test.
func captureLogOutput(t *testing.T) *bytes.Buffer {
	t.Helper()

	previous := logger.GetLogger()
	t.Cleanup(func() { logger.SetLogger(previous) })

	var buf bytes.Buffer
	redirected := zerolog.New(&buf)
	logger.SetLogger(&redirected)

	return &buf
}

// The property the adapter is shaped around: a capture that could not connect
// is a successful capture of a failure. A non-nil error would make WrapRun
// rewrite Result.Msg, burying the connect_error the file exists to record.
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

// The seam the dt value exists for: the artifact reaches the receiver under
// dt=pgMeta and nothing else. A capture that could not connect is uploaded like
// any other - the connect_error is the finding, and suppressing the upload
// would leave the run indistinguishable from one that never configured a block.
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

// Modelled on TestNodeDataTypeConstants: classification is an exact string
// match, and drift there drops the artifact silently.
func TestPostgresDataTypeConstant(t *testing.T) {
	// Must match the value the server team assigned for pg_metadata.txt
	// (direction §1.2).
	assert.Equal(t, "pgMeta", pgDTMetadata)

	// Every dt value the agent already uploads under. A collision does not
	// fail - it routes pg_metadata.txt into another artifact's receiver.
	taken := []string{
		"meta", "gc", "td", "hd", "ns", "df", "ps", "top", "vmstat", "dmesg",
		"agentlog", "cpuprofile", "kernel", "ping", "hdsub", "lp", "accessLog", "applog",
		nodeDTProcessOverview, nodeDTEventLoopLag, nodeDTUnhandledRejections,
		nodeDTModuleInventory, nodeDTHandleGrowth, nodeDTGCStats,
	}

	for _, dt := range taken {
		assert.NotEqual(t, dt, pgDTMetadata, "pgDTMetadata collides with an existing dt value")
	}
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

// A file-I/O failure is the only thing that makes Run return a non-nil error,
// and WrapRun answers it by logging the task with %#v into an agent log that is
// itself uploaded.
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

	// Buffered so the send in WrapRun's deferred block does not need a reader.
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

// A wiring mistake rather than a supported invocation: a nil dereference inside
// a capture goroutine takes the whole run down.
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
			// The classified case, whose value the artifact contract pins as a
			// bare token. Anything else here means the adapter formatted the
			// wrapped error instead of asking for the classification.
			name: "database at max_connections",
			metadata: postgres.Metadata{
				CaptureMode:  "unknown",
				ConnectError: "too_many_connections",
			},
			want: "pg_metadata.txt written; postgres connect failed: too_many_connections",
		},
		{
			// The normal least-privilege outcome on PostgreSQL 14-16, where
			// pg_monitor is not granted EXECUTE on pg_current_logfile().
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
