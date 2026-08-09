package postgres

import (
	"encoding/csv"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// artifactVersion is the format version carried in the block header. It changes
// when the key set or the value forms change in a way an existing reader would
// get wrong.
const artifactVersion = 1

// Every artifact in this package is a sequence of blocks: a header line - see
// writeBlockHeader - followed by a CSV body. The sampled artifacts write a
// tabular body per sample; pg_metadata.txt writes a key,value one per block.
//
// Every body goes through encoding/csv rather than a hand-rolled join:
// version() and shared_preload_libraries both contain commas in practice, and
// an identifier can contain anything at all.

// field is one key,value row of the body.
type field struct {
	key   string
	value string
}

// writeKeyValueBody renders a key,value block body: the column header, then one
// row per field. Every block carries its own column header, so a block is
// readable without the one before it.
func writeKeyValueBody(w io.Writer, fields []field) error {
	return writeFields(w, append([]field{{key: "key", value: "value"}}, fields...))
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

// serverBlockFields is the server block's body: the capture mode, then every
// server-derived row.
//
// There is deliberately no connect_error row. The block is written only where
// there was a connection, so the key could only ever be empty here; where it
// can be non-empty is the closing block's header, which the window writes.
func serverBlockFields(m Metadata) []field {
	return append([]field{{"capture_mode", m.CaptureMode}}, serverFields(m)...)
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

// headerField is one caller-supplied k=v token of a block header, in emission
// order.
type headerField struct {
	key   string
	value string
}

// writeBlockHeader renders one block header line:
//
//	# engine=postgres source=<source> v=<n> format=csv scope=<scope> [k=v ...] ts=<ts>
//
// The fixed keys bracket the caller's - engine, source, v, format and scope
// always lead and ts always closes - so a reader finds the block's identity and
// its clock read without parsing the middle.
//
// A key passed with an empty value is still written (dbid= before a connection
// exists), because for the body's every-key contract an empty value means "not
// read". Header keys, unlike body keys, may be conditional: sizes= and reason=
// appear only on a degraded sample and connect_error= only when there was no
// connection, and in each case absence is itself the value. A caller omits a
// key by not passing it, which is what makes a nil field list render with no
// stray separator.
//
// format is csv for every artifact today. It is a key rather than an assumption
// because the wire format is still open with the server team.
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

// maxHeaderValue bounds a header value, in runes, before quoting. singleLine
// flattens a driver message but does not bound it, and a PostgreSQL error
// carries DETAIL and HINT - a kilobyte has nowhere to wrap in header space. The
// cut is visible rather than silent: the ellipsis rides inside the quoted
// value.
const maxHeaderValue = 200

// headerValue renders a value as a header token.
//
// A value containing whitespace or a double quote is quoted, so driver text
// like `ERROR: canceling statement due to statement timeout` cannot break the
// header's space-delimited k=v tokenisation. Everything else - classified
// tokens, identifiers, numbers, timestamps - stays bare.
//
// strconv.QuoteToGraphic rather than strconv.Quote: Quote escapes non-ASCII to
// \uXXXX, and a database or relation name is user-chosen and may legally be
// non-ASCII - it should stay readable rather than arrive as escapes a non-Go
// parser has to decode.
//
// The parser rule this pins, stated once so it need not be inferred: split on
// unquoted whitespace; a value beginning with a double quote runs to the next
// unescaped double quote; the escaped forms inside it are Go's, the set
// strconv.Unquote accepts. That set is wider than the common cases - identifier
// text and driver text are both arbitrary, so \xNN, \uNNNN and \UNNNNNNNN all
// reach the artifact alongside \" \\ \t.
func headerValue(s string) string {
	s = truncateRunes(singleLine(s), maxHeaderValue)

	if needsHeaderQuoting(s) {
		return strconv.QuoteToGraphic(s)
	}

	return s
}

// needsHeaderQuoting reports whether s would break k=v tokenisation bare.
// Non-graphic runes are quoted too - they cannot be read back off a terminal
// otherwise, and quoting is what makes them visible as escapes.
func needsHeaderQuoting(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || !unicode.IsGraphic(r) {
			return true
		}
	}

	return false
}

// truncateRunes bounds s to n runes, marking that it was cut. Runes rather than
// bytes so a multi-byte character is never split in half.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[:n]) + "..."
}

// writeRows renders a tabular block body: the column header, then the rows.
//
// A column header with no rows writes the header and nothing else - the shape
// that distinguishes "captured and found nothing" from "could not be captured".
//
// Every cell goes through singleLine, not just the ones expected to need it: a
// quoted identifier may legally contain a line break.
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
