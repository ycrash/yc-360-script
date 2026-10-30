//go:build mysqlintegration

package mysql

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// The perms profile of compose.mysql.yaml: my84-perms writes its error log where a
// real installation does, in the data directory, 0640 and owned by the server's
// uid, with no bind mount.
//
//	docker compose -f compose.mysql.yaml --profile perms up -d --wait my84-perms
//
// From the host the agent cannot see that file at all; only beside the server can
// it be refused. scripts/mysql-perms-check.sh runs TestPermsBesideTheServer in a
// container that shares my84-perms's network and data directory, as a uid that
// is not the server's: once sharing its process table, once not.

// permsContainer is set by the script to shared-pid or own-pid.
const permsContainer = "YC_MY_PERMS_CONTAINER"

func TestPermsFromTheHostTheLogIsNotThere(t *testing.T) {
	if os.Getenv(permsContainer) != "" {
		t.Skip("runs on the host")
	}

	server := matrixServer{service: "my84-perms", port: 13386, variant: VariantMySQL}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", server.port), time.Second)
	if err != nil {
		t.Skip("the perms profile is not up")
	}

	conn.Close()

	files, _ := runMatrixWindow(t, matrixTarget(server, matrixRoles[0]), nil)
	metadata := files["my_metadata.txt"]

	assert.Contains(t, metadata, "\nlog_error_resolved,/var/lib/mysql/error.log\n")
	assert.Contains(t, metadata, "\nlog_access,none\n")
	assert.Contains(t, metadata, "\nlog_access_reason,unreadable\n")
	assert.Contains(t, metadata, "\nlog_open_error,not_found\n", "the server's path, absent from the agent's namespace")
}

func TestPermsBesideTheServer(t *testing.T) {
	mode := os.Getenv(permsContainer)
	if mode == "" {
		t.Skip("runs beside my84-perms: scripts/mysql-perms-check.sh")
	}

	server := matrixServer{service: "my84-perms", port: 3306, variant: VariantMySQL}
	db := rootDB(t, server)

	files, results := runMatrixWindow(t, matrixTarget(server, matrixRoles[0]), func() { forceDeadlock(t, db) })

	for _, result := range results {
		assert.Equal(t, StatusComplete, result.Status, "%s: %s", result.Artifact.FileName, result.Err)
	}

	metadata := files["my_metadata.txt"]

	assert.Contains(t, metadata, "\nlog_error_resolved,/var/lib/mysql/error.log\n")
	assert.Contains(t, metadata, "\nlog_access,none\n")
	assert.Contains(t, metadata, "\nlog_access_reason,unreadable\n")
	assert.Contains(t, metadata, "\nlog_open_error,permission_denied\n", "the file is there, 0640, and not ours")

	deadlocks := files["my_deadlocks.txt"]

	assert.Contains(t, deadlocks, " reason=log_unreadable ")
	assert.Contains(t, deadlocks, ` fallback="my_buffer_pool.txt:LATEST DETECTED DEADLOCK" `)
	assert.NotContains(t, deadlocks, " matched=", "nothing was read, so there is no count")

	bufferPool := files["my_buffer_pool.txt"]

	assert.Contains(t, bufferPool, "LATEST DETECTED DEADLOCK", "the fallback holds the deadlock the log could not give")
	assert.NotContains(t, bufferPool, "'dl'")
	assert.NotContains(t, bufferPool, "asc dl;")

	switch mode {
	case "shared-pid":
		// thread_start_time after the first boot, whose init the entrypoint ran
		// before exec'ing mysqld; either after a restart, a quick one.
		assert.Contains(t, metadata, "\nagent_on_db_host,yes\n")
		assert.Regexp(t, "\nagent_on_db_host_by,("+hostConfirmedByThreadStart+"|"+hostConfirmedByStartTime+")\n", metadata)
		assert.Contains(t, metadata, "\nhost_artifacts,captured\n")

	case "own-pid":
		assert.Contains(t, metadata, "\nagent_on_db_host,unknown\n")
		assert.Contains(t, metadata, "\nagent_on_db_host_reason,"+hostReasonContainer+"\n",
			"the agent's container hides the processes beside it")

	default:
		t.Fatalf("%s=%q: shared-pid or own-pid", permsContainer, mode)
	}
}
