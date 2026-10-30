//go:build mysqlintegration

package mysql

import (
	"database/sql"
	"fmt"
	"net"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The galera profile of compose.mysql.yaml: galera1 and galera2, a two-node
// MariaDB Galera cluster.
//
//	docker compose -f compose.mysql.yaml --profile galera up -d --wait galera1 galera2
//
// Skipped when it is not up. Each test leaves both nodes Synced in one Primary
// component.

type galeraNode struct {
	name   string
	port   int
	target Target
	root   *sql.DB
}

func requireGalera(t *testing.T) (galera1, galera2 galeraNode) {
	t.Helper()

	node := func(name string, port int) galeraNode {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
		if err != nil {
			t.Skip("the galera profile is not up")
		}

		conn.Close()

		cfg := gomysql.NewConfig()
		cfg.User, cfg.Passwd = "root", "yc-root-pw"
		cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("127.0.0.1:%d", port)

		connector, err := gomysql.NewConnector(cfg)
		require.NoError(t, err)

		root := sql.OpenDB(connector)
		t.Cleanup(func() { root.Close() })

		target := Target{ID: name, Host: "127.0.0.1", Port: port, Username: "root", Password: "yc-root-pw", TLS: TLS{Disabled: true}}

		return galeraNode{name: name, port: port, target: target, root: root}
	}

	galera1, galera2 = node("galera1", 13321), node("galera2", 13322)

	waitForCluster(t, galera1, galera2)

	return galera1, galera2
}

func wsrepStatusOf(t *testing.T, node galeraNode, name string) string {
	t.Helper()

	var value string

	err := node.root.QueryRow("SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(new(string), &value)
	require.NoError(t, err)

	return value
}

// waitForCluster waits until both nodes are Synced members of one Primary
// component of two.
func waitForCluster(t *testing.T, nodes ...galeraNode) {
	t.Helper()

	require.Eventually(t, func() bool {
		for _, node := range nodes {
			if wsrepStatusOf(t, node, "wsrep_cluster_status") != "Primary" ||
				wsrepStatusOf(t, node, "wsrep_cluster_size") != "2" ||
				wsrepStatusOf(t, node, "wsrep_local_state_comment") != "Synced" {
				return false
			}
		}

		return true
	}, 60*time.Second, 500*time.Millisecond, "the cluster never formed")
}

func TestGaleraNodesReadAsGalera(t *testing.T) {
	galera1, galera2 := requireGalera(t)

	for _, node := range []galeraNode{galera1, galera2} {
		file := replicationFileOf(t, node.target)

		assert.Contains(t, file, " mechanism=galera ", node.name)
		assert.Contains(t, file, "\nwsrep_provider_name,Galera\n", node.name)
		assert.Contains(t, file, "\nwsrep_cluster_status,Primary\n", node.name)
		assert.Contains(t, file, "\nwsrep_cluster_size,2\n", node.name)
	}
}

// TestGaleraNodeWithoutItsQuorumIsStillGalera cuts galera2 off. With two nodes,
// neither half has a quorum, and each says non-Primary and not ready: the state a
// report most needs to name, and one whose status values a standalone MariaDB's
// resemble. The provider still says Galera.
func TestGaleraNodeWithoutItsQuorumIsStillGalera(t *testing.T) {
	galera1, galera2 := requireGalera(t)

	_, err := galera2.root.Exec("SET GLOBAL wsrep_provider_options = 'gmcast.isolate=1'")
	require.NoError(t, err)

	t.Cleanup(func() {
		_, err := galera2.root.Exec("SET GLOBAL wsrep_provider_options = 'gmcast.isolate=0'")
		require.NoError(t, err)
		waitForCluster(t, galera1, galera2)
	})

	require.Eventually(t, func() bool {
		return wsrepStatusOf(t, galera2, "wsrep_cluster_status") == "non-Primary"
	}, 30*time.Second, 500*time.Millisecond)

	file := replicationFileOf(t, galera2.target)

	assert.Contains(t, file, " mechanism=galera ")
	assert.Contains(t, file, "\nwsrep_provider_name,Galera\n")
	assert.Contains(t, file, "\nwsrep_cluster_status,non-Primary\n")
	assert.Contains(t, file, "\nwsrep_ready,OFF\n")
}
