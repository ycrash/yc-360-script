//go:build mysqlintegration

package mysql

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Skipped unless up:
//
//	docker compose -f compose.mysql.yaml --profile mariadb up -d --wait mariadb1011
var mariadbServer = matrixServer{service: "mariadb1011", port: 13311, variant: VariantMariaDB}

// mariadbTarget is plaintext: the 10.11 image has TLS off.
func mariadbTarget(t *testing.T, role matrixRole) Target {
	t.Helper()

	conn, err := net.DialTimeout("tcp", "127.0.0.1:13311", time.Second)
	if err != nil {
		t.Skip("the mariadb profile is not up")
	}

	conn.Close()

	target := matrixTarget(mariadbServer, role)
	target.TLS = TLS{Disabled: true}

	return target
}

func TestMariaDBSessionIsBoundedUnderItsOwnNames(t *testing.T) {
	target := mariadbTarget(t, matrixRoles[0])

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err, "the second login, with MariaDB's names, completes")

	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, "SELECT @@max_statement_time, @@tx_read_only, @@lock_wait_timeout")
	require.NoError(t, err)

	defer rows.Close()

	require.True(t, rows.Next())

	var statementTime float64

	var readOnly, lockWait int

	require.NoError(t, rows.Scan(&statementTime, &readOnly, &lockWait))
	assert.Equal(t, StatementTimeout.Seconds(), statementTime)
	assert.Equal(t, 1, readOnly)
	assert.Equal(t, LockWaitTimeout, lockWait)
}

func TestMariaDBIsNamedAndItsGapsRecorded(t *testing.T) {
	for _, role := range matrixRoles {
		t.Run(role.user, func(t *testing.T) {
			files, results := runMatrixWindow(t, mariadbTarget(t, role), nil)

			for _, result := range results {
				assert.NotEqual(t, StatusConnectFailed, result.Status, "%s: %s", result.Artifact.FileName, result.Err)
			}

			assert.Contains(t, files["my_metadata.txt"], "\nmysql_variant,mariadb\n")

			if role.user != "root" {
				return
			}

			// Each refusal is its own block, not a failed run; without the grant, 1142 comes first.
			assert.Contains(t, files["my_lock_waits.txt"], "Table 'performance_schema.data_lock_waits' doesn't exist")
			assert.Contains(t, files["my_nondefault_settings.txt"], "Table 'performance_schema.variables_info' doesn't exist")
			assert.Contains(t, files["my_errors.txt"], "Table 'performance_schema.events_errors_summary_global_by_error' doesn't exist")
			assert.Contains(t, files["my_slow_queries.txt"], "Unknown column 'query_sample_text'")
		})
	}
}

func TestMariaDBWithoutAProviderIsNotGalera(t *testing.T) {
	file := replicationFileOf(t, mariadbTarget(t, matrixRoles[0]))

	assert.NotContains(t, file, "mechanism=galera")
	assert.Contains(t, file, "\nwsrep_cluster_status,Disconnected\n")
	assert.Contains(t, file, "\nwsrep_provider_name,\n")

	// No replication_group_members on MariaDB: the check says unknown.
	assert.Contains(t, file, " mechanism=unknown ")
}
