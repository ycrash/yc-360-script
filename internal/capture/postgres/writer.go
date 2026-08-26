package postgres

import (
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// artifactVersion changes when the key set or the value forms change in a way an
// existing reader would get wrong.
const artifactVersion = 1

// The two body formats; a receiver dispatches on format=, not
// filename. formatText's end is given by the block header's bytes= key, never
// by scanning for the next '#' — see logtail.go.
const (
	formatCSV  = "csv"
	formatText = "text"
)

type field struct {
	key   string
	value string
}

// writeKeyValueBody prepends the column header every block carries, so a block
// is readable without the one before it.
func writeKeyValueBody(w io.Writer, fields []field) error {
	return writeFields(w, append([]field{{key: "key", value: "value"}}, fields...))
}

// targetFields is what was configured; deliberately no password row.
func targetFields(m Metadata) []field {
	return []field{
		{"agent_ts", timestamp(m.AgentTS)},
		{"yc360_version", m.YC360Version},
		{"target_host", m.TargetHost},
		{"target_port", strconv.Itoa(m.TargetPort)},
		{"target_database", m.TargetDatabase},
		{"target_username", m.TargetUsername},
		{"target_sslmode", m.TargetSSLMode},

		// Policy, not readings: written here so a refused connection still records what
		// the run intended.
		{"explain_mode", m.ExplainMode},
		{"explain_literals", m.ExplainLiterals},
	}
}

// serverBlockFields deliberately has no connect_error row: this block is only
// written where there was a connection. connect_error appears in the closing
// block's header instead.
func serverBlockFields(m Metadata) []field {
	return append([]field{
		{"log_access", m.LogAccess},
		{"log_access_reason", m.LogAccessReason},
	}, serverFields(m)...)
}

// serverFields is every row requiring a connection, written whether or not the
// statement behind it succeeded.
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
		{"log_rotation_age", m.LogRotationAge},
		{"log_rotation_size", m.LogRotationSize},
		{"log_timezone", m.LogTimezone},
		{"log_min_messages", m.LogMinMessages},
		{"log_error_verbosity", m.LogErrorVerbosity},
		{"log_min_error_statement", m.LogMinErrorStatement},
		{"log_file_mode", m.LogFileMode},
		{"log_min_duration_statement", m.LogMinDurationStatement},
		{"log_parameter_max_length", m.LogParameterMaxLength},
		{"track_activity_query_size", m.TrackActivityQuerySize},
		{"track_io_timing", m.TrackIOTiming},
		{"pg_stat_statements.max", m.PgStatStatementsMax},
		{"pg_stat_statements.track", m.PgStatStatementsTrack},
		{"pg_stat_statements.track_planning", m.PgStatStatementsTrackPlanning},
		{"pg_stat_statements.track_utility", m.PgStatStatementsTrackUtility},
		{"auto_explain.log_min_duration", m.AutoExplainLogMinDuration},
		{"auto_explain.log_verbose", m.AutoExplainLogVerbose},
		{"auto_explain.log_analyze", m.AutoExplainLogAnalyze},
		{"auto_explain.log_format", m.AutoExplainLogFormat},
		{"auto_explain.sample_rate", m.AutoExplainSampleRate},
		{"update_process_title", m.UpdateProcessTitle},
		{"shared_preload_libraries", m.SharedPreloadLibraries},
		{"settings_unavailable", m.SettingsUnavailable},

		{"data_directory", m.DataDirectory},
		{"current_logfile", m.CurrentLogfile},
		{"current_logfile_resolved", m.CurrentLogfileResolved},
		{"log_resolved_by", m.LogResolvedBy},
		{"log_formats", m.LogFormats},

		{"agent_on_db_host", m.AgentOnDBHost},
		{"agent_on_db_host_by", m.AgentOnDBHostBy},
		{"agent_on_db_host_evidence", m.AgentOnDBHostEvidence},
		{"agent_on_db_host_reason", m.AgentOnDBHostReason},
		{"host_artifacts", m.HostArtifacts},

		{"has_pg_monitor_role", m.HasPgMonitorRole},
		{"has_pg_read_all_stats", m.HasPgReadAllStats},
		{"has_pg_stat_statements", m.HasPgStatStatements},
		{"pg_stat_statements_version", m.PgStatStatementsVersion},
		{"has_pg_stat_checkpointer", m.HasPgStatCheckpointer},
		{"has_generic_plan", m.HasGenericPlan},
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

type headerField struct {
	key   string
	value string
}

// writeBlockHeader renders one block header line:
//
//	# engine=postgres source=<source> v=<n> format=csv scope=<scope> [k=v ...] ts=<ts>
//
// An empty value means "not read" (e.g. dbid= before a connection exists); a
// missing key (error=, connect_error=) means the thing it describes didn't
// happen. Always format=csv; a text body calls writeBlockHeaderFormat directly.
func writeBlockHeader(w io.Writer, source, scope string, fields []headerField, ts time.Time) error {
	return writeBlockHeaderFormat(w, source, scope, formatCSV, fields, ts)
}

func writeBlockHeaderFormat(w io.Writer, source, scope, format string, fields []headerField, ts time.Time) error {
	tokens := make([]string, 0, len(fields)+6)

	tokens = append(tokens,
		"engine=postgres",
		"source="+headerValue(source),
		"v="+strconv.Itoa(artifactVersion),
		"format="+headerValue(format),
		"scope="+headerValue(scope),
	)

	for _, f := range fields {
		tokens = append(tokens, f.key+"="+headerValue(f.value))
	}

	tokens = append(tokens, "ts="+timestamp(ts))

	_, err := io.WriteString(w, "# "+strings.Join(tokens, " ")+"\n")

	return err
}

// maxHeaderValue bounds a header value in runes: a PostgreSQL error carries
// DETAIL and HINT, and a kilobyte has nowhere to wrap in header space.
const maxHeaderValue = 200

// headerValue quotes anything that would break space-delimited k=v tokenisation.
// QuoteToGraphic, not Quote: Quote escapes non-ASCII to \uXXXX, and identifiers
// may legally be non-ASCII. Parser rule: split on unquoted whitespace; a value
// starting with " runs to the next unescaped ", per strconv.Unquote's escapes.
func headerValue(s string) string {
	s = truncateRunes(singleLine(s), maxHeaderValue)

	if needsHeaderQuoting(s) {
		return strconv.QuoteToGraphic(s)
	}

	return s
}

// needsHeaderQuoting also quotes non-graphic runes, which makes them visible as
// escapes rather than unreadable off a terminal.
func needsHeaderQuoting(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || !unicode.IsGraphic(r) {
			return true
		}
	}

	return false
}

// truncateRunes marks the cut, and counts runes so a multi-byte character is
// never split in half.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[:n]) + "..."
}

// writeRows writes the column header even with no rows, the shape that
// distinguishes "captured and found nothing" from "could not be captured".
func writeRows(w io.Writer, columns []string, rows [][]string) error {
	cw := csv.NewWriter(w)

	if err := cw.Write(singleLineAll(columns)); err != nil {
		return err
	}

	for _, row := range rows {
		if err := cw.Write(singleLineAll(row)); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

func singleLineAll(cells []string) []string {
	flattened := make([]string, len(cells))
	for i, cell := range cells {
		flattened[i] = singleLine(cell)
	}

	return flattened
}

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

// singleLine collapses line breaks so one row is one line - not for the CSV
// encoding (which would quote a newline fine) but for the artifact's claim that
// it needs no record-aware parsing. The one place the agent mutates a captured value.
func singleLine(s string) string {
	return lineBreaks.Replace(s)
}

// timestamp renders an agent-side clock read in the artifact's timestamp form.
func timestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
