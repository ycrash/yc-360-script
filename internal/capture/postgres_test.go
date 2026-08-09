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

// readPostgresArtifact reads pg_metadata.txt's key,value rows, merged across
// its blocks: the artifact's contract is one key set per file, and which block
// a key lives in is the postgres package's business rather than this one's.
//
// The block headers are split off by their leading # before the body is parsed,
// rather than being read as one-field CSV records. A header value is quoted
// where it needs to be, and real driver text carries commas and quotes: a
// connect_error= header is not a CSV record and must not be handed to a CSV
// reader.
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

		if record[0] == "key" {
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

	taken := []string{
		"meta", "gc", "td", "hd", "ns", "df", "ps", "top", "vmstat", "dmesg",
		"agentlog", "cpuprofile", "kernel", "ping", "hdsub", "lp", "accessLog", "applog",
		nodeDTProcessOverview, nodeDTEventLoopLag, nodeDTUnhandledRejections,
		nodeDTModuleInventory, nodeDTHandleGrowth, nodeDTGCStats,
	}

	for _, dt := range taken {
		assert.NotEqual(t, dt, pgDTMetadata, "pgDTMetadata collides with an existing dt value")
		assert.NotEqual(t, dt, pgDTBloat, "pgDTBloat collides with an existing dt value")
		assert.NotEqual(t, dt, pgDTHealth, "pgDTHealth collides with an existing dt value")
	}

	assert.NotEqual(t, pgDTMetadata, pgDTBloat, "two postgres artifacts share a dt")
	assert.NotEqual(t, pgDTMetadata, pgDTHealth, "two postgres artifacts share a dt")
	assert.NotEqual(t, pgDTBloat, pgDTHealth, "two postgres artifacts share a dt")
}

func TestPostgresBundleFileNames(t *testing.T) {
	assert.Equal(t, "pg_metadata.txt", PostgresMetadataFileName)
	assert.Equal(t, "pg_health.txt", PostgresHealthFileName)
	assert.Equal(t, "pg_bloat.txt", PostgresBloatFileName)

	seen := map[string]bool{}
	for _, name := range postgresArtifactFiles {
		assert.True(t, strings.HasPrefix(name, "pg_"),
			"%s: database artifacts keep engine-specific prefixes, pg_ here", name)

		assert.NotContains(t, name, ".appLogs.",
			"%s is classified by its exact name, not by the app-log substring convention", name)

		assert.False(t, seen[name], "%s is used for two artifacts", name)
		seen[name] = true
	}

	assert.Len(t, seen, 3, "every artifact the run writes is named here")
}

func TestPostgresSampledDataTypeGate(t *testing.T) {
	assert.Equal(t, pgDTMetadata, pgSampledDataType(pgMetadataCollector().Artifact()),
		"pg_metadata.txt is routed through the same gate as every other artifact")

	assert.Equal(t, pgDTBloat, pgSampledDataType(postgres.Bloat{}.Artifact()),
		"pg_bloat.txt has its assigned value")

	assert.Equal(t, pgDTHealth, pgSampledDataType(postgres.Health{}.Artifact()),
		"pg_health.txt has its assigned value")

	assert.Empty(t, pgSampledDataType(postgres.Artifact{Name: "pg_replication"}),
		"an artifact the server team has not assigned a dt for is not uploaded")
}

func pgMetadataCollector() *postgres.MetadataCollector {
	return postgres.NewMetadata(postgres.Target{}, "3.6.1", time.Now())
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

	return readSampledArtifact(t, PostgresBloatFileName)
}

func readSampledArtifact(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(name)
	require.NoError(t, err)

	return string(content)
}

// postgresArtifactFiles is every file one run writes, in registration order.
var postgresArtifactFiles = []string{
	PostgresHealthFileName,
	PostgresMetadataFileName,
	PostgresBloatFileName,
}

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

	assert.Contains(t, result.Msg, PostgresHealthFileName+" written (0/12 samples)",
		"twelve samples expected at 10s over a 2m window, none taken")
	assert.Contains(t, result.Msg, PostgresBloatFileName+" written (0/2 samples)")
	assert.Contains(t, result.Msg, PostgresMetadataFileName+" written; postgres connect failed",
		"all three artifacts report the one refusal, and they report it identically")

	_, values := readPostgresArtifact(t)
	assert.Equal(t, "127.0.0.1", values["target_host"],
		"the target block is on disk even though the connection never happened")
	assert.NotContains(t, values, "current_database",
		"and the server block is not, because there was no server to read")
}

// TestPostgresCaptureOpensOneConnectionPerRun is the assertion this slice
// exists to make: before it, the metadata task and the window dialled
// independently, so a cluster at max_connections could refuse whichever lost
// the race - a failure the run itself caused.
//
// The listener accepts and then says nothing, so each dial costs
// ConnectTimeout and this test with it. That is the price of counting dials
// rather than TCP connections: a listener that closes at once makes pgx retry
// the startup with a lower protocol version, which is two accepts for one dial
// and would make the count mean nothing.
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
	target.SSLMode = "disable"

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

	require.Len(t, uploads, 3, "every artifact with an assigned dt is uploaded")

	byDT := map[string]string{}
	for _, upload := range uploads {
		byDT[upload.dt] = upload.body
	}

	require.Contains(t, byDT, pgDTMetadata, "pg_metadata.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTHealth, "pg_health.txt uploaded under the wrong dt")
	require.Contains(t, byDT, pgDTBloat, "pg_bloat.txt uploaded under the wrong dt")

	for dt, source := range map[string]string{
		pgDTMetadata: "source=pg_metadata",
		pgDTHealth:   "source=pg_health",
		pgDTBloat:    "source=pg_bloat",
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

	assert.Equal(t, 2, strings.Count(result.Msg, " | "),
		"one run-level record, three summaries joined into it")

	assert.NotContains(t, result.Msg, "not uploaded: dt value not yet assigned",
		"no artifact takes the skip path now that all three dt values are assigned")

	assert.True(t, result.Ok,
		"all three artifacts transmitted, so the task's Ok must be true")
}

// TestPostgresCaptureMetadataLineKeepsItsProbeClauses is what stops the
// metadata artifact's run-log line regressing to a sample count. Collect never
// returns an error, so a metadata sample cannot be anything but complete: with
// the count alone, a capture whose every probe was denied would read as
// "(1/1 samples)" and say nothing at all.
func TestPostgresCaptureMetadataLineKeepsItsProbeClauses(t *testing.T) {
	collected := postgres.Metadata{
		CaptureMode:         "pg-remote",
		CurrentLogfileError: "ERROR: permission denied for function pg_current_logfile (SQLSTATE 42501)",
	}

	artifact := postgres.ArtifactResult{
		Artifact:        pgMetadataCollector().Artifact(),
		Status:          postgres.StatusComplete,
		SamplesExpected: 1,
		SamplesWritten:  1,
	}

	summary := postgresArtifactSummary(artifact, collected)

	assert.Contains(t, summary, PostgresMetadataFileName+" written (mode=pg-remote)",
		"the mode, not the count")
	assert.Contains(t, summary, "pg_current_logfile failed",
		"and the probe clause, which a sample count would have swallowed whole")
	assert.NotContains(t, summary, "samples")

	artifact.Status = postgres.StatusConnectFailed
	artifact.SamplesWritten = 0
	artifact.Err = "too_many_connections"

	assert.Equal(t,
		PostgresMetadataFileName+" written; postgres connect failed: too_many_connections",
		postgresArtifactSummary(artifact, postgres.Metadata{CaptureMode: "unknown"}),
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

	// A directory where the artifact wants to be: os.Create fails on that one
	// file and on no other.
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
	target.SSLMode = "disable"

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

	assert.Equal(t, config.DefaultPostgresCaptureDuration, task.captureDuration())

	window := config.Duration(45 * time.Second)
	task.Target.CaptureDuration = &window
	assert.Equal(t, 45*time.Second, task.captureDuration())
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, postgresArtifactMessage(tt.result))
		})
	}
}
