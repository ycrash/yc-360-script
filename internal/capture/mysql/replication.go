package mysql

import (
	"bytes"
	"context"
	"io"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// Replication mechanisms, as the replication block's mechanism= names them.
const (
	mechanismAsync  = "async"
	mechanismGroup  = "group_replication"
	mechanismGalera = "galera"
	mechanismNone   = "none"

	// mechanismUnknown: a check that would have found it was refused or failed.
	mechanismUnknown = "unknown"
)

// replicaErrorColumns quote the failing statement and its values - "Duplicate entry
// '42' for key ... Query: 'INSERT ...'" - and have them replaced.
var replicaErrorColumns = []string{"Last_Error", "Last_SQL_Error", "Last_IO_Error"}

// wsrepStatus are the Galera variables the spec names; wsrep_flow_control_paused is
// the one to watch.
var wsrepStatus = []string{"wsrep_cluster_size", "wsrep_cluster_status", "wsrep_connected", "wsrep_ready",
	"wsrep_local_state_comment", "wsrep_flow_control_paused", "wsrep_local_recv_queue_avg"}

// Replication is my_replication.txt. Three mechanisms exclude one another, checked
// in the spec's order on every sample: a replica's status; failing that, the Group
// Replication members; failing that, Galera's status variables, which exist only on
// a Galera node. Then the binary log's position and retention, whatever the
// mechanism.
type Replication struct {
	Interval time.Duration

	state blockState
}

func (r *Replication) Artifact() Artifact {
	return periodicArtifact("my_replication", ConnectionNormal, r.Interval)
}

func (r *Replication) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	artifact := r.Artifact()
	version := s.Engine.Version

	var buf bytes.Buffer

	// SHOW REPLICA STATUS is 8.0.22; older servers know only SHOW SLAVE STATUS.
	replicaStatus := "SHOW REPLICA STATUS"
	if version.known() && !version.AtLeast(8, 0, 22) {
		replicaStatus = "SHOW SLAVE STATUS"
	}

	mechanism := mechanismNone
	unread := false

	found, read, err := r.write(ctx, q, &buf, artifact, s, block{
		source:           "replica_status",
		query:            replicaStatus,
		statementColumns: replicaErrorColumns,
	})
	if err != nil {
		return err
	}

	unread = unread || !read

	if found {
		mechanism = mechanismAsync
	}

	if mechanism == mechanismNone {
		found, read, err = r.write(ctx, q, &buf, artifact, s, block{
			source: "replication_group_members",
			query:  "SELECT * FROM performance_schema.replication_group_members",
		})
		if err != nil {
			return err
		}

		unread = unread || !read

		if found {
			mechanism = mechanismGroup
		}
	}

	if mechanism == mechanismNone {
		found, read, err = r.write(ctx, q, &buf, artifact, s, block{
			source:  "wsrep_status",
			query:   showGlobal("STATUS", wsrepStatus...),
			columns: globalStatusColumns,
		})
		if err != nil {
			return err
		}

		unread = unread || !read

		if found {
			mechanism = mechanismGalera
		}
	}

	if mechanism == mechanismNone && unread {
		mechanism = mechanismUnknown
	}

	// SHOW BINARY LOG STATUS is 8.2; 8.4 removed SHOW MASTER STATUS. An unread
	// version takes the older form, which every server before 8.4 has.
	binlogStatus := "SHOW MASTER STATUS"
	if version.AtLeast(8, 2, 0) {
		binlogStatus = "SHOW BINARY LOG STATUS"
	}

	if _, _, err := r.write(ctx, q, &buf, artifact, s, block{
		source: "binary_log_status",
		query:  binlogStatus,
		fields: []dbcore.HeaderField{kv("mechanism", mechanism)},
	}); err != nil {
		return err
	}

	if _, _, err := r.write(ctx, q, &buf, artifact, s, block{
		source:  "binary_log_variables",
		query:   showGlobal("VARIABLES", "log_bin", "binlog_expire_logs_seconds", "gtid_mode", "server_uuid"),
		columns: globalStatusColumns,
	}); err != nil {
		return err
	}

	_, err = w.Write(buf.Bytes())

	return err
}

// write writes one block, and reports whether it found rows and whether it was read
// at all: refused or failed, it says nothing about the mechanism.
func (r *Replication) write(ctx context.Context, q Querier, w io.Writer, artifact Artifact, s SampleContext,
	b block,
) (found, read bool, err error) {
	outcome, err := writeBlock(ctx, q, w, artifact, s, &r.state, b)

	return outcome.rows > 0, outcome.read, err
}
