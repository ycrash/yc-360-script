package postgres

import (
	"strconv"
	"time"
)

// Runs against PG 14-18 as CONNECT-only. Capabilities are probed from the
// catalog, not server_version_num (pg_upgrade can carry an old extension's
// pre-upgrade columns forward); settings come from pg_settings, not
// SHOW/current_setting(), which stays silent instead of raising on an
// unknown or superuser-only name.

// timestampLayout is RFC 3339 in UTC, to milliseconds.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// capturedSettings pairs name and destination so the server list, the
// assignment, and settings_unavailable can't drift apart.
var capturedSettings = []struct {
	name  string
	field func(*Metadata) *string
}{
	{"max_connections", func(m *Metadata) *string { return &m.MaxConnections }},
	{"logging_collector", func(m *Metadata) *string { return &m.LoggingCollector }},
	{"log_destination", func(m *Metadata) *string { return &m.LogDestination }},
	// Gates pg_checkpoint_log.txt: the server writes the line it tails only
	// under on, the default since 15 and off on 14. Read so an empty tail can
	// say whether the server was ever asked to log checkpoints.
	{"log_checkpoints", func(m *Metadata) *string { return &m.LogCheckpoints }},
	// superuser-only
	{"log_directory", func(m *Metadata) *string { return &m.LogDirectory }},
	// superuser-only
	{"log_filename", func(m *Metadata) *string { return &m.LogFilename }},
	{"log_line_prefix", func(m *Metadata) *string { return &m.LogLinePrefix }},
	// log_rotation_age is minutes, log_rotation_size is kB (1440/10240 by
	// default, measured on all five). The rest gate what reaches the log.
	{"log_rotation_age", func(m *Metadata) *string { return &m.LogRotationAge }},
	{"log_rotation_size", func(m *Metadata) *string { return &m.LogRotationSize }},
	{"log_timezone", func(m *Metadata) *string { return &m.LogTimezone }},
	{"log_min_messages", func(m *Metadata) *string { return &m.LogMinMessages }},
	{"log_error_verbosity", func(m *Metadata) *string { return &m.LogErrorVerbosity }},
	{"log_min_error_statement", func(m *Metadata) *string { return &m.LogMinErrorStatement }},
	{"log_file_mode", func(m *Metadata) *string { return &m.LogFileMode }},
	{"log_min_duration_statement", func(m *Metadata) *string { return &m.LogMinDurationStatement }},
	{"log_parameter_max_length", func(m *Metadata) *string { return &m.LogParameterMaxLength }},
	// Governs where pg_sessions.txt's query column truncates - mid-token and
	// unmarked.
	{"track_activity_query_size", func(m *Metadata) *string { return &m.TrackActivityQuerySize }},
	// track_io_timing/track_planning default off, zeroing several
	// pg_slow_queries.txt columns. The pg_stat_statements.* GUCs below exist
	// only while the library is preloaded - absent here means that, not denial.
	{"track_io_timing", func(m *Metadata) *string { return &m.TrackIOTiming }},
	{"pg_stat_statements.max", func(m *Metadata) *string { return &m.PgStatStatementsMax }},
	{"pg_stat_statements.track", func(m *Metadata) *string { return &m.PgStatStatementsTrack }},
	{"pg_stat_statements.track_planning", func(m *Metadata) *string { return &m.PgStatStatementsTrackPlanning }},
	{"pg_stat_statements.track_utility", func(m *Metadata) *string { return &m.PgStatStatementsTrackUtility }},
	// The auto_explain.* GUCs exist only while the module is loaded in this session:
	// absent means not loaded, not denied. Between them they say whether the LOGGED mode
	// can yield anything, and whether its entries carry the join key.
	{"auto_explain.log_min_duration", func(m *Metadata) *string { return &m.AutoExplainLogMinDuration }},
	{"auto_explain.log_verbose", func(m *Metadata) *string { return &m.AutoExplainLogVerbose }},
	{"auto_explain.log_analyze", func(m *Metadata) *string { return &m.AutoExplainLogAnalyze }},
	{"auto_explain.log_format", func(m *Metadata) *string { return &m.AutoExplainLogFormat }},
	{"auto_explain.sample_rate", func(m *Metadata) *string { return &m.AutoExplainSampleRate }},
	// Required by the same-host check: without it the check cannot tell a suppressed
	// process title from another machine's process.
	{"update_process_title", func(m *Metadata) *string { return &m.UpdateProcessTitle }},
	// superuser-only
	{"shared_preload_libraries", func(m *Metadata) *string { return &m.SharedPreloadLibraries }},
	{"compute_query_id", func(m *Metadata) *string { return &m.ComputeQueryID }},
	// superuser-only. Not read via logLocationSQL: pg_monitor lacks the
	// pg_current_logfile() grant on 14-16.
	{"data_directory", func(m *Metadata) *string { return &m.DataDirectory }},
}

func settingNames() []string {
	names := make([]string, len(capturedSettings))
	for i, s := range capturedSettings {
		names[i] = s.name
	}

	return names
}

// Settings are in pg_settings.setting form - internal units, so `500` not
// SHOW's `500ms`. Name/value arrays, not a 2-D array or json, pass text
// through without a UTF-8 conversion that could fail on SQL_ASCII;
// host(inet_server_addr()) avoids the /32 a cast would render.
//
// pg_monitor probes 'member'. pg_read_all_stats
// probes 'usage' instead: it predicts pg_stat_activity's masking, gated on
// privilege inheritance since PG15 (a NOINHERIT member reads true under
// 'member' but is denied the query), and 'usage' only safely under-claims on
// 14. Do not harmonise the two.
const serverFactsSQL = `WITH s AS (
    SELECT name, COALESCE(setting, '') AS setting
      FROM pg_catalog.pg_settings
     WHERE name = ANY($1::text[])
)
SELECT
    current_database()::text,
    current_user::text,
    version(),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'server_version_num'),
    pg_is_in_recovery(),
    pg_postmaster_start_time(),
    pg_backend_pid(),
    host(inet_server_addr()),
    inet_server_port(),
    host(inet_client_addr()),
    inet_client_port(),
    (SELECT stats_reset FROM pg_catalog.pg_stat_database WHERE datname = current_database()),
    (SELECT array_agg(name ORDER BY name) FROM s),
    (SELECT array_agg(setting ORDER BY name) FROM s),
    to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL,
    ` + genericPlanSQL + `,
    EXISTS (
        SELECT 1
          FROM pg_catalog.pg_attribute
         WHERE attrelid = to_regclass('pg_catalog.pg_stat_database')
           AND attname = 'sessions_fatal'
           AND NOT attisdropped
    ),
    COALESCE((SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'pg_stat_statements'), ''),
    pg_has_role(current_user, 'pg_monitor', 'member'),
    pg_has_role(current_user, 'pg_read_all_stats', 'usage'),
    now(),
    clock_timestamp()`

// pg_current_logfile's grant to pg_monitor only landed in PostgreSQL 17: on
// 14-16 denial is the normal outcome for the recommended role.
const logLocationSQL = `SELECT pg_current_logfile()`

// replicationSQL is separated defensively, though it needs no grant:
// pg_stat_replication masks columns, never rows.
const replicationSQL = `SELECT count(*) FROM pg_catalog.pg_stat_replication`

// Every scalar is a pointer so an unexpected NULL can't turn into a scan
// error that loses the whole statement; NULL arrays scan into nil natively.
type serverFactsRow struct {
	currentDatabase  *string
	currentUser      *string
	version          *string
	serverVersionNum *string
	isInRecovery     *bool
	postmasterStart  *time.Time
	backendPID       *int32
	inetServerAddr   *string
	inetServerPort   *int32
	inetClientAddr   *string
	inetClientPort   *int32
	statsReset       *time.Time
	settingNames     []string
	settingValues    []string
	hasCheckpointer  *bool
	hasGenericPlan   *bool
	hasSessionFatal  *bool
	pgStatStatements *string
	hasPgMonitorRole *bool
	hasPgReadAllStat *bool
	serverNow        *time.Time
	serverClock      *time.Time
}

// dest is in serverFactsSQL's selection order.
func (r *serverFactsRow) dest() []any {
	return []any{
		&r.currentDatabase,
		&r.currentUser,
		&r.version,
		&r.serverVersionNum,
		&r.isInRecovery,
		&r.postmasterStart,
		&r.backendPID,
		&r.inetServerAddr,
		&r.inetServerPort,
		&r.inetClientAddr,
		&r.inetClientPort,
		&r.statsReset,
		&r.settingNames,
		&r.settingValues,
		&r.hasCheckpointer,
		&r.hasGenericPlan,
		&r.hasSessionFatal,
		&r.pgStatStatements,
		&r.hasPgMonitorRole,
		&r.hasPgReadAllStat,
		&r.serverNow,
		&r.serverClock,
	}
}

// settings pairs the two aggregate columns back into a map. The length guard
// makes an edit that breaks their correspondence produce missing settings
// rather than mismatched ones.
func (r *serverFactsRow) settings() map[string]string {
	if len(r.settingNames) != len(r.settingValues) {
		return nil
	}

	out := make(map[string]string, len(r.settingNames))
	for i, name := range r.settingNames {
		out[name] = r.settingValues[i]
	}

	return out
}

// text records NULL and empty identically: empty means "not read", and the
// *_error keys say why.
func text(v *string) string {
	if v == nil {
		return ""
	}

	return *v
}

func boolText(v *bool) string {
	if v == nil {
		return ""
	}

	return strconv.FormatBool(*v)
}

func int32Text(v *int32) string {
	if v == nil {
		return ""
	}

	return strconv.FormatInt(int64(*v), 10)
}

// int64Text is empty, never 0: empty means "not read" where 0 is a reading.
func int64Text(v *int64) string {
	if v == nil {
		return ""
	}

	return strconv.FormatInt(*v, 10)
}

// float64Text carries int64Text's rule. Precision -1 renders the shortest form
// that round-trips, so 0.4 does not arrive as 0.40000000000000002.
func float64Text(v *float64) string {
	if v == nil {
		return ""
	}

	return strconv.FormatFloat(*v, 'f', -1, 64)
}

// timeText renders a nullable timestamp as read, in UTC.
func timeText(v *time.Time) string {
	if v == nil {
		return ""
	}

	return timestamp(*v)
}
