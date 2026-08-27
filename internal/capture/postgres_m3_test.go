package capture

import (
	"os"
	"strings"
	"testing"

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
	assert.Contains(t, result.Msg, "dt value not yet assigned")
	assert.False(t, result.Ok)

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

func TestRunnerName(t *testing.T) {
	hostname, err := os.Hostname()
	require.NoError(t, err)

	assert.Equal(t, hostname, runnerName())
}
