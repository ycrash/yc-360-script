package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// defaultArtifactVersion is v= for an artifact that sets no Version. An artifact's version changes
// when its key set or value forms change in a way an existing reader would get wrong.
const defaultArtifactVersion = 1

// sampleHeaderVersion is v= for a periodic file whose one break is the two-line sample
// header.
const sampleHeaderVersion = 2

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
		{"target_tls_enabled", m.TargetTLSEnabled},
		{"target_tls_verify", m.TargetTLSVerify},
		{"target_tls_ca_file", m.TargetTLSCAFile},
		{"target_tls_server_name", m.TargetTLSServerName},

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
		{"uptime_seconds", m.UptimeSeconds},
		{"stats_reset", m.StatsReset},
		{"version", m.Version},
		{"server_version_num", m.ServerVersionNum},

		{"max_connections", m.MaxConnections},
		{"logging_collector", m.LoggingCollector},
		{"log_destination", m.LogDestination},
		{"log_checkpoints", m.LogCheckpoints},
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

		// host_artifacts said again as a true|false flag: whether this capture's
		// host files describe the database's machine. Derived here, so the two
		// cannot disagree.
		{"host_metrics_available", strconv.FormatBool(m.HostArtifacts == HostArtifactsCaptured)},

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
		{"agent_ts_at_clock_read", clockRead(m.AgentTSAtClockRead)},
		{"clock_read_rtt_ms", m.ClockReadRTTMS},
		{"connect_ms", m.ConnectMS},
	}
}

// tablespaceLocationColumns heads pg_metadata.txt's one tabular block: a
// key,value body would make a tablespace's name a key, and a name is data.
var tablespaceLocationColumns = []string{"spcname", "location"}

// writeTablespaceBlock writes the tablespaces with storage of their own, one
// row each, and a column header alone where there are none or the read failed;
// the failure rides the header as error=, the way pg_capacity.txt's WAL block
// carries its refusal.
func writeTablespaceBlock(w io.Writer, header []headerField, m Metadata, at time.Time) error {
	fields := append([]headerField{}, header...)
	if m.TablespaceError != "" {
		fields = append(fields, headerField{"error", m.TablespaceError})
	}

	var block bytes.Buffer

	if err := writeBlockHeader(&block, "pg_metadata_tablespaces", metadataScope, fields, at); err != nil {
		return err
	}

	rows := make([][]string, len(m.Tablespaces))
	for i, tablespace := range m.Tablespaces {
		rows[i] = []string{tablespace.Name, tablespace.Location}
	}

	if err := writeRows(&block, tablespaceLocationColumns, rows); err != nil {
		return err
	}

	_, err := w.Write(block.Bytes())

	return err
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
	return writeVersionedBlockHeader(w, source, defaultArtifactVersion, scope, format, fields, ts)
}

// writeVersionedBlockHeader is for an artifact whose Version is its own.
func writeVersionedBlockHeader(w io.Writer, source string, version int, scope, format string,
	fields []headerField, ts time.Time,
) error {
	line := make([]headerField, 0, len(fields)+6)
	line = append(line,
		headerField{"engine", "postgres"},
		headerField{"source", source},
		headerField{"v", strconv.Itoa(version)},
		headerField{"format", format},
		headerField{"scope", scope},
	)
	line = append(line, fields...)
	line = append(line, headerField{"ts", timestamp(ts)})

	return writeHeaderLine(w, line)
}

func writeHeaderLine(w io.Writer, fields []headerField) error {
	tokens := make([]string, len(fields))
	for i, f := range fields {
		tokens[i] = f.key + "=" + headerValue(f.value)
	}

	_, err := io.WriteString(w, "# "+strings.Join(tokens, " ")+"\n")

	return err
}

// A sample block's status=: its reads succeeded, or found by design that there was
// nothing to read (reason=); a read failed; a read ran out of time; a plan was cut at
// its size cap.
const (
	statusOK        = "OK"
	statusError     = "ERROR"
	statusTimeout   = "TIMEOUT"
	statusTruncated = "TRUNCATED"
)

// queryCanceled is the server's statement timeout, and a cancelled statement.
const queryCanceled = "57014"

// readStatus is the status of a block whose read ended in err.
func readStatus(err error) string {
	switch {
	case err == nil:
		return statusOK

	case hasSQLState(err, queryCanceled), errors.Is(err, context.DeadlineExceeded):
		return statusTimeout
	}

	return statusError
}

// span is when a block's statements ran, from the first one's start to the last one's
// end. Zero when none ran.
type span struct {
	start time.Time
	end   time.Time
}

// sampleHeader is what a sample block's second header line says about the block.
type sampleHeader struct {
	source string

	// scope is the artifact's when empty.
	scope string

	reads span

	// status is readStatus's, or statusTruncated.
	status string

	rows      int
	truncated bool

	// fields are the block's own keys, written after the envelope's.
	fields []headerField
}

// writeSampleHeader writes a periodic file's two header lines for one sample block:
//
//	# capture_id=<uuid> target_id=<id> engine=postgres engine_version=<n> v=<n> format=<f> db=<db> dbid=<oid>
//	# sample_id=<n> source=<source> start_ts=<ts> end_ts=<ts> duration_ms=<n> status=<status> rows=<n> truncated=<bool> scope=<scope> [k=v ...] ts=<ts>
//
// The first line is the same on every sample block of a file, the second is the
// block's own; on each, the envelope's keys come first. The times are empty on a block
// no statement was run for.
func writeSampleHeader(w io.Writer, artifact Artifact, s SampleContext, h sampleHeader) error {
	if err := writeHeaderLine(w, []headerField{
		{"capture_id", s.CaptureID},
		{"target_id", s.TargetID},
		{"engine", "postgres"},
		{"engine_version", s.EngineVersion},
		{"v", strconv.Itoa(artifactVersion(artifact))},
		{"format", artifactFormat(artifact)},
		{"db", s.Database},
		{"dbid", s.DBID},
	}); err != nil {
		return err
	}

	scope := h.scope
	if scope == "" {
		scope = artifact.Scope
	}

	fields := make([]headerField, 0, len(h.fields)+10)
	fields = append(fields,
		headerField{"sample_id", strconv.Itoa(s.Index)},
		headerField{"source", h.source},
		headerField{"start_ts", clockRead(h.reads.start)},
		headerField{"end_ts", clockRead(h.reads.end)},
		headerField{"duration_ms", durationMillis(h.reads)},
		headerField{"status", h.status},
		headerField{"rows", strconv.Itoa(h.rows)},
		headerField{"truncated", strconv.FormatBool(h.truncated)},
		headerField{"scope", scope},
	)
	fields = append(fields, h.fields...)
	fields = append(fields, headerField{"ts", timestamp(s.At)})

	return writeHeaderLine(w, fields)
}

// durationMillis is a span's length in whole milliseconds, empty where nothing ran.
func durationMillis(reads span) string {
	if reads.start.IsZero() {
		return ""
	}

	return strconv.FormatInt(reads.end.Sub(reads.start).Milliseconds(), 10)
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

// clockRead renders a reading that may not have happened: zero is empty, not year
// one, which would read as two thousand years of clock skew.
func clockRead(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return timestamp(t)
}

// millisText renders milliseconds - finer than the networks measured, coarse
// enough to stay stable. An unmeasured duration is empty, like every unread value.
func millisText(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	return strconv.FormatFloat(d.Seconds()*1000, 'f', 1, 64)
}
