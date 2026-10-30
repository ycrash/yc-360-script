package mysql

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// replicationServer is a server with no replica channel and no Group Replication,
// answering the wsrep_ read with rows.
func replicationServer(wsrep [][]any) *fakeQuerier {
	empty := fakeResult{columns: []string{"Variable_name", "Value"}}

	return (&fakeQuerier{}).
		on("SHOW REPLICA STATUS", fakeResult{columns: []string{"Replica_IO_State"}}).
		on("replication_group_members", fakeResult{columns: []string{"MEMBER_ID"}}).
		on("wsrep_", fakeResult{columns: []string{"Variable_name", "Value"}, rows: wsrep}).
		on("SHOW BINARY LOG STATUS", fakeResult{columns: []string{"File"}}).
		on("log_bin", empty)
}

func TestGaleraIsAProviderLoadedNotAnyWsrepRow(t *testing.T) {
	// As the servers returned them, measured on MariaDB 10.11.6 and a two-node
	// Galera cluster of it.
	tests := []struct {
		name  string
		wsrep [][]any
		want  string
	}{
		{"a standalone MariaDB: the base rows, and no provider", [][]any{
			{"wsrep_cluster_size", "0"}, {"wsrep_cluster_status", "Disconnected"},
			{"wsrep_connected", "OFF"}, {"wsrep_ready", "OFF"}, {"wsrep_provider_name", ""},
		}, mechanismNone},
		{"a Galera node", [][]any{
			{"wsrep_local_recv_queue_avg", "0.166667"}, {"wsrep_flow_control_paused", "0"},
			{"wsrep_local_state_comment", "Synced"}, {"wsrep_cluster_size", "2"},
			{"wsrep_cluster_status", "Primary"}, {"wsrep_connected", "ON"}, {"wsrep_ready", "ON"},
			{"wsrep_provider_name", "Galera"},
		}, mechanismGalera},
		{"a Galera node that has lost its quorum", [][]any{
			{"wsrep_local_recv_queue_avg", "0.2"}, {"wsrep_flow_control_paused", "0"},
			{"wsrep_local_state_comment", "Initialized"}, {"wsrep_cluster_size", "1"},
			{"wsrep_cluster_status", "non-Primary"}, {"wsrep_connected", "ON"}, {"wsrep_ready", "OFF"},
			{"wsrep_provider_name", "Galera"},
		}, mechanismGalera},
		{"MySQL and Percona Server: none at all", nil, mechanismNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			require.NoError(t, (&Replication{}).Sample(context.Background(), replicationServer(tt.wsrep), &out, testSample(1, 1)))

			assert.Contains(t, out.String(), " mechanism="+tt.want+" ")
		})
	}
}
