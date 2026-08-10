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

// Every artifact is a sequence of blocks: a header line, then a CSV body. Bodies
// go through encoding/csv rather than a hand-rolled join - version() and
// shared_preload_libraries contain commas in practice, and an identifier can
// contain anything at all.

type field struct {
	key   string
	value string
}

// writeKeyValueBody prepends the column header every block carries, so a block
// is readable without the one before it.
func writeKeyValueBody(w io.Writer, fields []field) error {
	return writeFields(w, append([]field{{key: "key", value: "value"}}, fields...))
}

// targetFields is what was configured. There is deliberately no password row.
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

// serverBlockFields has deliberately no connect_error row: this block is written
// only where there was a connection, so the key could only ever be empty. Where
// it can be non-empty is the closing block's header, which the window writes.
func serverBlockFields(m Metadata) []field {
	return append([]field{{"capture_mode", m.CaptureMode}}, serverFields(m)...)
}

// serverFields is every row requiring a connection, in artifact order. All are
// written whether or not the statement behind them succeeded.
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

type headerField struct {
	key   string
	value string
}

// writeBlockHeader renders one block header line:
//
//	# engine=postgres source=<source> v=<n> format=csv scope=<scope> [k=v ...] ts=<ts>
//
// The fixed keys bracket the caller's, so a reader finds the block's identity
// and its clock read without parsing the middle. An empty value is still written
// (dbid= before a connection exists) and means "not read". Header keys, unlike
// body keys, may also be conditional - error= and connect_error= appear only
// when what they describe happened, and absence is itself the value.
func writeBlockHeader(w io.Writer, source, scope string, fields []headerField, ts time.Time) error {
	tokens := make([]string, 0, len(fields)+6)

	tokens = append(tokens,
		"engine=postgres",
		"source="+headerValue(source),
		"v="+strconv.Itoa(artifactVersion),
		"format=csv",
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

// headerValue quotes anything that would break the space-delimited k=v
// tokenisation. QuoteToGraphic rather than Quote: Quote escapes non-ASCII to
// \uXXXX, and a database or relation name may legally be non-ASCII.
//
// The parser rule this pins: split on unquoted whitespace, and a value beginning
// with a double quote runs to the next unescaped one, escaped by the set
// strconv.Unquote accepts.
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

// writeRows writes the column header even with no rows - the shape that
// distinguishes "captured and found nothing" from "could not be captured".
// Every cell goes through singleLine: a quoted identifier may contain a break.
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

// singleLine collapses line breaks so one row is one line. Not about the
// encoding - encoding/csv would quote an embedded newline correctly - but about
// the artifact's claim that it needs no record-aware parsing. This is the one
// place the agent mutates a captured value.
func singleLine(s string) string {
	return lineBreaks.Replace(s)
}

// timestamp renders an agent-side clock read in the artifact's timestamp form.
func timestamp(t time.Time) string {
	return t.UTC().Format(timestampLayout)
}
