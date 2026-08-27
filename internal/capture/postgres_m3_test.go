package capture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readPollPayload(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile(postgresM3OutputPath)
	require.NoError(t, err)

	return string(content)
}

// A database that cannot be reached is the most important reading of all, so the
// poll is still written and still sent - v1's rule of leaving rows out would have
// made an outage look like a passing blip.
func TestPostgresM3RecordsAnUnreachableDatabase(t *testing.T) {
	chdirToCaptureDir(t)

	task := &PostgresM3{Target: unreachablePostgres(t)}

	result, err := task.Run()
	require.NoError(t, err, "a refused connection is a successful capture of a failure")

	payload := readPollPayload(t)

	assert.Contains(t, payload, "source=pg_m3")
	assert.Contains(t, payload, "agent_on_db_host=unknown")
	assert.Contains(t, payload, "agent_on_db_host_reason=no_connection")
	assert.Contains(t, payload, "heartbeat_error,unreachable")
	assert.Contains(t, payload, "disk_reason,no_connection")

	// The runner-health pair stands in for the top capture that will not run.
	assert.Contains(t, payload, "agent_cpu_pct,")

	assert.False(t, task.OnDBHost(), "no connection can never confirm the database host")
	// The reading is sent whatever it found: an outage is the reading that matters
	// most, and withholding it would make one look like a passing blip.
	assert.NotEmpty(t, result.Msg)

	// The password never reaches the payload, the header or the target rows.
	assert.NotContains(t, payload, pgTestPassword)
}

// The declaration exists for exactly this case: the database is down, so there is
// no backend to look for, and the host readings are all the evidence there is.
func TestPostgresM3HonoursTheDeclarationWhenTheDatabaseIsDown(t *testing.T) {
	chdirToCaptureDir(t)

	target := unreachablePostgres(t)
	target.AgentOnDBHost = true

	task := &PostgresM3{Target: target}

	_, err := task.Run()
	require.NoError(t, err)

	payload := readPollPayload(t)

	assert.Contains(t, payload, "agent_on_db_host=yes")
	assert.Contains(t, payload, "agent_on_db_host_by=configured")
	assert.True(t, task.OnDBHost(), "the top capture runs on the operator's declaration")

	// Nothing is kept between cycles, so a poll that never connected has no
	// data_directory to read - the declaration cannot conjure one.
	assert.Contains(t, payload, "disk_reason,no_connection")

	// Runner health belongs to a runner that is not the database host.
	assert.NotContains(t, payload, "agent_cpu_pct,")
}

func TestPostgresM3WithoutATargetDoesNothing(t *testing.T) {
	chdirToCaptureDir(t)

	result, err := (&PostgresM3{}).Run()
	require.NoError(t, err)

	assert.Contains(t, result.Msg, "no postgres block configured")

	_, err = os.Stat(postgresM3OutputPath)
	assert.True(t, os.IsNotExist(err), "no target, no payload")
}

// The header names the runner and the target, which is what lets the server spot
// two runners polling one database.
func TestPostgresM3HeaderIdentifiesRunnerAndTarget(t *testing.T) {
	chdirToCaptureDir(t)

	target := unreachablePostgres(t)
	target.Database = "orders_db"

	_, err := (&PostgresM3{Target: target}).Run()
	require.NoError(t, err)

	header := strings.SplitN(readPollPayload(t), "\n", 2)[0]

	hostname, err := os.Hostname()
	require.NoError(t, err)

	assert.Contains(t, header, "runner="+hostname)
	assert.Contains(t, header, "target_host=127.0.0.1")
	assert.Contains(t, header, "target_database=orders_db")
}

// The provisional dt is the one the receiver is being asked to assign; it must
// reach the wire under its own value and share with none of the other ten.
func TestPostgresM3UploadsUnderItsOwnDT(t *testing.T) {
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

	task := &PostgresM3{Target: unreachablePostgres(t)}
	task.SetEndpoint(server.URL + "?de=test")

	result, err := task.Run()
	require.NoError(t, err)
	assert.True(t, result.Ok)

	mu.Lock()
	defer mu.Unlock()

	require.Len(t, uploads, 1, "one reading, one upload")
	assert.Equal(t, "pgM3", uploads[0].dt)
	assert.Contains(t, uploads[0].body, "source=pg_m3", "the receiver is handed the payload, not an empty body")
	assert.NotContains(t, uploads[0].body, pgTestPassword)
}

func TestRunnerName(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	assert.Equal(t, hostname, runnerName())
}
