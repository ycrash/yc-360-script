package postgres

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// defaultArtifactVersion is v= for an artifact that sets no Version. An artifact's version changes
// when its key set or value forms change in a way an existing reader would get wrong.
const defaultArtifactVersion = dbcore.DefaultArtifactVersion

// sampleHeaderVersion is v= for a periodic file whose one break is the two-line sample
// header.
const sampleHeaderVersion = dbcore.SampleHeaderVersion

// The two body formats; a receiver dispatches on format=, not
// filename. formatText's end is given by the block header's bytes= key, never
// by scanning for the next '#' — see logtail.go.
const (
	formatCSV  = dbcore.FormatCSV
	formatText = dbcore.FormatText
)

type field struct {
	key   string
	value string
}

// coreFields is fields as dbcore writes them.
func coreFields(fields []field) []dbcore.Field {
	converted := make([]dbcore.Field, len(fields))
	for i, f := range fields {
		converted[i] = dbcore.Field{Key: f.key, Value: f.value}
	}

	return converted
}

// writeKeyValueBody prepends the column header every block carries, so a block
// is readable without the one before it.
func writeKeyValueBody(w io.Writer, fields []field) error {
	return dbcore.WriteKeyValueBody(w, coreFields(fields))
}

// targetFields is what was configured; deliberately no password row.
func targetFields(m Metadata) []field {
	return []field{
		{"agent_ts", timestamp(m.AgentTS)},
		{"yc360_version", m.YC360Version},
		{"target_id", m.TargetID},
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

// coreHeader is fields as dbcore writes them.
func coreHeader(fields []headerField) []dbcore.HeaderField {
	converted := make([]dbcore.HeaderField, len(fields))
	for i, f := range fields {
		converted[i] = dbcore.HeaderField{Key: f.key, Value: f.value}
	}

	return converted
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
	return dbcore.WriteBlockHeader(w, engineName, source, version, scope, format, coreHeader(fields), ts)
}

// A sample block's status=: see dbcore.
const (
	statusOK        = dbcore.StatusOK
	statusError     = dbcore.StatusError
	statusTimeout   = dbcore.StatusTimeout
	statusTruncated = dbcore.StatusTruncated
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
	return dbcore.WriteSampleHeader(w, engineName, location(s), artifact, s, dbcore.SampleHeader{
		Source:    h.source,
		Scope:     h.scope,
		Reads:     dbcore.Span{Start: h.reads.start, End: h.reads.end},
		Status:    h.status,
		Rows:      h.rows,
		Truncated: h.truncated,
		Fields:    coreHeader(h.fields),
	})
}

// maxHeaderValue bounds a header value in runes: a PostgreSQL error carries
// DETAIL and HINT, and a kilobyte has nowhere to wrap in header space.
const maxHeaderValue = dbcore.MaxHeaderValue

// headerValue quotes anything that would break space-delimited k=v tokenisation.
func headerValue(s string) string { return dbcore.HeaderValue(s) }

// truncateRunes marks the cut, and counts runes so a multi-byte character is
// never split in half.
func truncateRunes(s string, n int) string { return dbcore.TruncateRunes(s, n) }

// writeRows writes the column header even with no rows, the shape that
// distinguishes "captured and found nothing" from "could not be captured".
func writeRows(w io.Writer, columns []string, rows [][]string) error {
	return dbcore.WriteRows(w, columns, rows)
}

// singleLine collapses line breaks so one row is one line.
func singleLine(s string) string { return dbcore.SingleLine(s) }

// timestamp renders an agent-side clock read in the artifact's timestamp form.
func timestamp(t time.Time) string { return dbcore.Timestamp(t) }

// clockRead renders a reading that may not have happened: zero is empty, not year
// one, which would read as two thousand years of clock skew.
func clockRead(t time.Time) string { return dbcore.ClockRead(t) }

// millisText renders milliseconds - finer than the networks measured, coarse
// enough to stay stable. An unmeasured duration is empty, like every unread value.
func millisText(d time.Duration) string { return dbcore.MillisText(d) }
