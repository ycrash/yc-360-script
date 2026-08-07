package postgres

import (
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// artifactVersion is the format version carried in the block header. It changes
// when the key set or the value forms change in a way an existing reader would
// get wrong.
const artifactVersion = 1

// The artifact is a block header, a CSV column header, and a fixed sequence of
// key,value rows, written by two entry points rather than one - see WriteTarget.
//
// The body goes through encoding/csv rather than a hand-rolled join: version()
// and shared_preload_libraries both contain commas in practice.

// field is one key,value row of the body.
type field struct {
	key   string
	value string
}

// WriteTarget emits the block header, the column header, and everything
// knowable from configuration before a connection exists.
//
// It is called - and synced - before Connect, so every failure path leaves a
// file, including the process being killed mid-connect on a wedged host. It is
// also why the artifact is never zero bytes, and so is never dropped by the
// upload path's empty-file check.
func WriteTarget(w io.Writer, m Metadata) error {
	header := fmt.Sprintf(
		"# engine=postgres source=pg_metadata v=%d format=csv scope=cluster ts=%s\n",
		artifactVersion, timestamp(m.AgentTS),
	)

	if _, err := io.WriteString(w, header); err != nil {
		return err
	}

	// Through the same path as the body, so one set of quoting rules covers the
	// whole file.
	return writeFields(w, append([]field{{key: "key", value: "value"}}, targetFields(m)...))
}

// WriteResult appends the outcome: the capture mode, the connection error, and
// - when a connection existed - every server-derived row.
//
// A non-empty connect_error tells a reader the file stops there. Otherwise
// every key is written, empty where the read failed, so nobody has to decide
// whether an absent key means an old agent or a failed query.
func WriteResult(w io.Writer, m Metadata) error {
	return writeFields(w, resultFields(m))
}

// targetFields is what was configured, as opposed to what the server said.
// There is deliberately no target_password row.
func targetFields(m Metadata) []field {
	return []field{
		{"agent_ts", timestamp(m.AgentTS)},
		{"yc360_version", m.YC360Version},
		{"target_host", m.TargetHost},
		{"target_port", strconv.Itoa(m.TargetPort)},
		{"target_database", m.TargetDatabase},
		{"target_username", m.TargetUsername},
		{"target_sslmode", m.TargetSSLMode},
	}
}

func resultFields(m Metadata) []field {
	fields := []field{
		{"capture_mode", m.CaptureMode},
		{"connect_error", m.ConnectError},
	}

	if m.ConnectError != "" {
		return fields
	}

	return append(fields, serverFields(m)...)
}

// serverFields is every row that requires a connection, in artifact order. All
// are written whether or not the statement behind them succeeded; the *_error
// rows say which read failed.
func serverFields(m Metadata) []field {
	return []field{
		{"current_database", m.CurrentDatabase},
		{"current_user", m.CurrentUser},
		{"backend_pid", m.BackendPID},
		{"inet_server_addr", m.InetServerAddr},
		{"inet_server_port", m.InetServerPort},
		{"is_in_recovery", m.IsInRecovery},
		{"postmaster_start_time", m.PostmasterStartTime},
		{"stats_reset", m.StatsReset},
		{"version", m.Version},
		{"server_version_num", m.ServerVersionNum},

		{"max_connections", m.MaxConnections},
		{"logging_collector", m.LoggingCollector},
		{"log_destination", m.LogDestination},
		{"log_directory", m.LogDirectory},
		{"log_filename", m.LogFilename},
		{"log_line_prefix", m.LogLinePrefix},
		{"log_min_duration_statement", m.LogMinDurationStatement},
		{"log_parameter_max_length", m.LogParameterMaxLength},
		{"shared_preload_libraries", m.SharedPreloadLibraries},
		{"settings_unavailable", m.SettingsUnavailable},

		{"data_directory", m.DataDirectory},
		{"current_logfile", m.CurrentLogfile},
		{"current_logfile_resolved", m.CurrentLogfileResolved},
		{"current_logfile_readable", m.CurrentLogfileReadable},
		{"current_logfile_error", m.CurrentLogfileError},

		{"has_pg_monitor_role", m.HasPgMonitorRole},
		{"has_pg_stat_statements", m.HasPgStatStatements},
		{"pg_stat_statements_version", m.PgStatStatementsVersion},
		{"has_pg_stat_checkpointer", m.HasPgStatCheckpointer},
		{"has_session_fatal_stats", m.HasSessionFatalStats},
		{"compute_query_id", m.ComputeQueryID},

		{"replication_configured", m.ReplicationConfigured},
		{"replication_probe_error", m.ReplicationProbeError},

		{"query_error", m.QueryError},
		{"server_now", m.ServerNow},
		{"server_clock_timestamp", m.ServerClockTimestamp},
		{"agent_ts_at_clock_read", timestamp(m.AgentTSAtClockRead)},
	}
}

// writeFields renders rows as CSV records, flattening each value first.
func writeFields(w io.Writer, fields []field) error {
	cw := csv.NewWriter(w)

	for _, f := range fields {
		if err := cw.Write([]string{f.key, singleLine(f.value)}); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// singleLine collapses line breaks so one row is one line.
//
// Not about the encoding - encoding/csv would quote an embedded newline
// correctly - but about the artifact's claim that it needs no record-aware
// parsing, which otherwise depends on which error the driver returned. The cost
// is that this is the one place the agent mutates a captured value.
func singleLine(s string) string {
	return lineBreaks.Replace(s)
}

// timestamp renders an agent-side clock read in the artifact's timestamp form.
func timestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
