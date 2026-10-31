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

// Two-node Galera clusters from compose.mysql.yaml, skipped when not up:
//
//	docker compose -f compose.mysql.yaml --profile galera up -d --wait galera1 galera2
//	docker compose -f compose.mysql.yaml --profile pxc up -d --wait pxc1 pxc2

type galeraCluster struct {
	profile string
	nodes   [2]galeraNodeSpec

	// plaintext: MariaDB starts with TLS off; PXC's caching_sha2_password root needs TLS.
	plaintext bool
}

type galeraNodeSpec struct {
	name string
	port int
}

var galeraClusters = []galeraCluster{
	{profile: "galera", nodes: [2]galeraNodeSpec{{"galera1", 13321}, {"galera2", 13322}}, plaintext: true},
	{profile: "pxc", nodes: [2]galeraNodeSpec{{"pxc1", 13331}, {"pxc2", 13332}}},
}

type galeraNode struct {
	name   string
	target Target
	root   *sql.DB
}

func eachGaleraCluster(t *testing.T, test func(t *testing.T, nodes [2]galeraNode)) {
	for _, cluster := range galeraClusters {
		t.Run(cluster.profile, func(t *testing.T) {
			var nodes [2]galeraNode

			for i, spec := range cluster.nodes {
				nodes[i] = connectGaleraNode(t, cluster, spec)
			}

			waitForCluster(t, nodes[:]...)
			test(t, nodes)
		})
	}
}

func connectGaleraNode(t *testing.T, cluster galeraCluster, spec galeraNodeSpec) galeraNode {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", spec.port), time.Second)
	if err != nil {
		t.Skipf("the %s profile is not up", cluster.profile)
	}

	conn.Close()

	cfg := gomysql.NewConfig()
	cfg.User, cfg.Passwd = "root", "yc-root-pw"
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("127.0.0.1:%d", spec.port)
	cfg.TLSConfig = "skip-verify"

	if cluster.plaintext {
		cfg.TLSConfig = "false"
	}

	connector, err := gomysql.NewConnector(cfg)
	require.NoError(t, err)

	root := sql.OpenDB(connector)
	t.Cleanup(func() { root.Close() })

	target := Target{ID: spec.name, Host: "127.0.0.1", Port: spec.port, Username: "root", Password: "yc-root-pw"}
	target.TLS.Disabled = cluster.plaintext

	return galeraNode{name: spec.name, target: target, root: root}
}

func wsrepStatusOf(t *testing.T, node galeraNode, name string) string {
	t.Helper()

	var value string

	err := node.root.QueryRow("SHOW GLOBAL STATUS LIKE '"+name+"'").Scan(new(string), &value)
	require.NoError(t, err)

	return value
}

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
	eachGaleraCluster(t, func(t *testing.T, nodes [2]galeraNode) {
		for _, node := range nodes {
			file := replicationFileOf(t, node.target)

			assert.Contains(t, file, " mechanism=galera ", node.name)
			assert.Contains(t, file, "\nwsrep_provider_name,Galera\n", node.name)
			assert.Contains(t, file, "\nwsrep_cluster_status,Primary\n", node.name)
			assert.Contains(t, file, "\nwsrep_cluster_size,2\n", node.name)
		}
	})
}

func TestGaleraNodeWithoutItsQuorumIsStillGalera(t *testing.T) {
	eachGaleraCluster(t, func(t *testing.T, nodes [2]galeraNode) {
		cut := nodes[1]

		_, err := cut.root.Exec("SET GLOBAL wsrep_provider_options = 'gmcast.isolate=1'")
		require.NoError(t, err)

		t.Cleanup(func() {
			_, err := cut.root.Exec("SET GLOBAL wsrep_provider_options = 'gmcast.isolate=0'")
			require.NoError(t, err)
			waitForCluster(t, nodes[:]...)
		})

		require.Eventually(t, func() bool {
			return wsrepStatusOf(t, cut, "wsrep_cluster_status") == "non-Primary"
		}, 30*time.Second, 500*time.Millisecond)

		file := replicationFileOf(t, cut.target)

		assert.Contains(t, file, " mechanism=galera ")
		assert.Contains(t, file, "\nwsrep_provider_name,Galera\n")
		assert.Contains(t, file, "\nwsrep_cluster_status,non-Primary\n")
		assert.Contains(t, file, "\nwsrep_ready,OFF\n")
	})
}
