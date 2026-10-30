//go:build mysqlintegration

package mysql

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The replica profile of compose.mysql.yaml: my84-replica, a GTID async replica of
// my84.
//
//	docker compose -f compose.mysql.yaml --profile replica up -d --wait my84-replica
//
// Skipped when it is not up. Each test leaves the replica running and caught up.
var replicaServer = matrixServer{service: "my84-replica", port: 13385, variant: VariantMySQL}

func requireReplica(t *testing.T) (replica, source *sql.DB) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", replicaServer.port), time.Second)
	if err != nil {
		t.Skip("the replica profile is not up")
	}

	conn.Close()

	replica, source = rootDB(t, replicaServer), rootDB(t, matrixServers[2])
	waitForReplica(t, replica, caughtUp)

	return replica, source
}

// replicationFile runs my_replication.txt alone over a short window, as yc_monitor.
func replicationFile(t *testing.T, server matrixServer) string {
	t.Helper()

	return replicationFileOf(t, matrixTarget(server, matrixRoles[1]))
}

func replicationFileOf(t *testing.T, target Target) string {
	t.Helper()
	t.Chdir(t.TempDir())

	window := NewWindow(target, 3*time.Second, []Collector{&Replication{Interval: 2 * time.Second}}, "")

	for _, result := range window.Run(context.Background()) {
		if result.File != nil {
			result.File.Close()
		}

		require.Equal(t, StatusComplete, result.Status, result.Err)
	}

	content, err := os.ReadFile("my_replication.txt")
	require.NoError(t, err)

	return string(content)
}

// firstReplicaStatus is the first sample's SHOW REPLICA STATUS row by column, empty
// when it had none.
func firstReplicaStatus(t *testing.T, file string) map[string]string {
	t.Helper()

	_, body, found := strings.Cut(file, " source=replica_status ")
	require.True(t, found, "no replica_status block")

	var csvLines []string

	for _, line := range strings.Split(body, "\n")[1:] {
		if strings.HasPrefix(line, "# ") {
			break
		}

		csvLines = append(csvLines, line)
	}

	records, err := csv.NewReader(strings.NewReader(strings.Join(csvLines, "\n"))).ReadAll()
	require.NoError(t, err)

	row := map[string]string{}

	if len(records) > 1 {
		for i, column := range records[0] {
			row[column] = records[1][i]
		}
	}

	return row
}

// serverReplicaStatus is SHOW REPLICA STATUS as root reads it.
func serverReplicaStatus(t *testing.T, replica *sql.DB) map[string]string {
	t.Helper()

	rows, err := replica.Query("SHOW REPLICA STATUS")
	require.NoError(t, err)

	defer rows.Close()

	columns, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next(), "the replica has a channel")

	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))

	for i := range values {
		dest[i] = &values[i]
	}

	require.NoError(t, rows.Scan(dest...))

	status := map[string]string{}
	for i, column := range columns {
		status[column] = values[i].String
	}

	return status
}

func caughtUp(status map[string]string) bool {
	return status["Replica_IO_Running"] == "Yes" && status["Replica_SQL_Running"] == "Yes" &&
		status["Seconds_Behind_Source"] == "0" && status["Retrieved_Gtid_Set"] != "" &&
		strings.HasPrefix(status["Executed_Gtid_Set"], status["Retrieved_Gtid_Set"])
}

func waitForReplica(t *testing.T, replica *sql.DB, done func(map[string]string) bool) map[string]string {
	t.Helper()

	var status map[string]string

	require.Eventually(t, func() bool {
		status = serverReplicaStatus(t, replica)

		return done(status)
	}, 30*time.Second, 200*time.Millisecond, "the replica never got there: %v", status)

	return status
}

func TestReplicaRunningIsAsyncWithItsLag(t *testing.T) {
	requireReplica(t)

	file := replicationFile(t, replicaServer)
	status := firstReplicaStatus(t, file)

	assert.Contains(t, file, " mechanism=async ")
	assert.Equal(t, "my84", status["Source_Host"])
	assert.Equal(t, "Yes", status["Replica_IO_Running"])
	assert.Equal(t, "Yes", status["Replica_SQL_Running"])
	assert.Equal(t, "0", status["Seconds_Behind_Source"])

	// The source has a replica but is none, by the spec's test: mechanism is what
	// this server replicates from.
	file = replicationFile(t, matrixServers[2])

	assert.Contains(t, file, " mechanism=none ")
	assert.Empty(t, firstReplicaStatus(t, file))
	assert.Regexp(t, `source=binary_log_status [^\n]* status=OK rows=1 `, file)
}

func TestReplicaWithItsApplierStoppedHasNoLag(t *testing.T) {
	replica, _ := requireReplica(t)

	_, err := replica.Exec("STOP REPLICA SQL_THREAD")
	require.NoError(t, err)

	t.Cleanup(func() {
		_, err := replica.Exec("START REPLICA SQL_THREAD")
		require.NoError(t, err)
		waitForReplica(t, replica, caughtUp)
	})

	file := replicationFile(t, replicaServer)
	status := firstReplicaStatus(t, file)

	assert.Contains(t, file, " mechanism=async ", "a stopped applier is still a replica")
	assert.Equal(t, "Yes", status["Replica_IO_Running"])
	assert.Equal(t, "No", status["Replica_SQL_Running"])
	assert.Empty(t, status["Seconds_Behind_Source"], "NULL: no lag is known")
}

// TestReplicaErrorsKeepTheirDiagnosticsAndLoseTheirValues breaks replication the
// way an application write on a replica does: a row inserted there, then the same
// key on the source. Each applier and log format words the failure differently.
func TestReplicaErrorsKeepTheirDiagnosticsAndLoseTheirValues(t *testing.T) {
	replica, source := requireReplica(t)

	tests := []struct {
		name    string
		workers int
		format  string
		id      int
		kept    *regexp.Regexp

		// redacted is the count each sample's block gives: none for a message that
		// names only the transaction.
		redacted string
	}{
		{"parallel applier, row events", 4, "ROW", 774421,
			regexp.MustCompile(`Worker \d+ failed executing transaction '[0-9a-f-]{36}:\d+' at source log binlog\.\d+, end_log_pos \d+\.`), "0"},
		{"single applier, row events", 0, "ROW", 774422,
			regexp.MustCompile(`Could not execute Write_rows event on table yc_app\.orders; Duplicate entry '<redacted>', ` +
				`Error_code: 1062; handler error HA_ERR_FOUND_DUPP_KEY; the event's source log binlog\.\d+, end_log_pos \d+`), "2"},
		{"single applier, statements", 0, "STATEMENT", 774423,
			regexp.MustCompile(regexp.QuoteMeta(`Error '<redacted>'. Query: 'INSERT INTO yc_app.orders (id, customer_id, status) ` +
				`VALUES (<redacted>, <redacted>, '<redacted>')'`)), "8"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := fmt.Sprintf("yc-secret-%d", tt.id)

			_, err := replica.Exec("STOP REPLICA SQL_THREAD")
			require.NoError(t, err)

			_, err = replica.Exec(fmt.Sprintf("SET GLOBAL replica_parallel_workers = %d", tt.workers))
			require.NoError(t, err)

			t.Cleanup(func() {
				for _, statement := range []string{
					"STOP REPLICA SQL_THREAD",
					fmt.Sprintf("DELETE FROM yc_app.orders WHERE id = %d", tt.id),
					"SET GLOBAL replica_parallel_workers = 4",
					"START REPLICA SQL_THREAD",
				} {
					_, err := replica.Exec(statement)
					require.NoError(t, err, statement)
				}

				waitForReplica(t, replica, caughtUp)

				_, err := source.Exec(fmt.Sprintf("DELETE FROM yc_app.orders WHERE id = %d", tt.id))
				require.NoError(t, err)

				waitForReplica(t, replica, caughtUp)
			})

			_, err = replica.Exec("START REPLICA SQL_THREAD")
			require.NoError(t, err)

			_, err = replica.Exec(fmt.Sprintf(
				"INSERT INTO yc_app.orders (id, customer_id, status) VALUES (%d, 1, 'yc-replica-only')", tt.id))
			require.NoError(t, err)

			conn, err := source.Conn(context.Background())
			require.NoError(t, err)

			defer conn.Close()

			_, err = conn.ExecContext(context.Background(), "SET SESSION binlog_format = '"+tt.format+"'")
			require.NoError(t, err)

			_, err = conn.ExecContext(context.Background(), fmt.Sprintf(
				"INSERT INTO yc_app.orders (id, customer_id, status) VALUES (%d, 2, '%s')", tt.id, secret))
			require.NoError(t, err)

			waitForReplica(t, replica, func(status map[string]string) bool {
				return status["Last_SQL_Errno"] == "1062"
			})

			file := replicationFile(t, replicaServer)
			status := firstReplicaStatus(t, file)

			assert.NotContains(t, file, secret)
			assert.NotContains(t, file, fmt.Sprint(tt.id))
			assert.Equal(t, "1062", status["Last_SQL_Errno"])
			assert.Equal(t, "No", status["Replica_SQL_Running"])
			assert.Regexp(t, tt.kept, status["Last_SQL_Error"])
			assert.Equal(t, status["Last_SQL_Error"], status["Last_Error"])
			assert.Regexp(t, `source=replica_status [^\n]* redacted=`+tt.redacted+` `, file,
				"two columns, Last_Error and Last_SQL_Error, counted together")
		})
	}
}
