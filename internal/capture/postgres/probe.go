package postgres

import (
	"strconv"
	"time"
)

// Two rules for every statement here, because the capture runs against 14-18 as
// a role that may hold nothing beyond CONNECT.
//
// A capability is asked of the catalog, never derived from server_version_num:
// pg_upgrade carries an old extension schema forward, so a PG15 server can
// legitimately expose pg_stat_statements' pre-1.8 columns.
//
// Settings come from pg_settings, never SHOW or current_setting(). The view
// returns no row for an unknown name where SHOW raises, and omits a
// superuser-only setting where current_setting(name, true) would still raise.

// timestampLayout is RFC 3339 in UTC, to milliseconds.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// capturedSettings is what serverFactsSQL reads from pg_settings, in artifact
// order. Name and destination are paired here so the list sent to the server,
// the assignment and the settings_unavailable roll-up cannot drift apart.
var capturedSettings = []struct {
	name  string
	field func(*Metadata) *string
}{
	{"max_connections", func(m *Metadata) *string { return &m.MaxConnections }},
	{"logging_collector", func(m *Metadata) *string { return &m.LoggingCollector }},
	{"log_destination", func(m *Metadata) *string { return &m.LogDestination }},
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
	// superuser-only
	{"shared_preload_libraries", func(m *Metadata) *string { return &m.SharedPreloadLibraries }},
	{"compute_query_id", func(m *Metadata) *string { return &m.ComputeQueryID }},
	// superuser-only. Not in logLocationSQL: on 14-16 pg_monitor is denied
	// pg_current_logfile(), and this row would go down with that statement.
	{"data_directory", func(m *Metadata) *string { return &m.DataDirectory }},
}

func settingNames() []string {
	names := make([]string, len(capturedSettings))
	for i, s := range capturedSettings {
		names[i] = s.name
	}

	return names
}

// serverFactsSQL is the unprivileged statement: identity, run facts, the
// settings catalogue and every capability probe. No path in it can raise for a
// role holding only CONNECT. Settings are in pg_settings.setting form - internal
// units, so `500` rather than SHOW's `500ms`.
//
// Parallel name and value arrays rather than a 2-D array or a json aggregate:
// text arrays pass bytes through without a UTF-8 conversion that could fail on a
// SQL_ASCII cluster. host(inet_server_addr()) rather than a cast, which would
// render 172.17.0.2/32.
//
// The two role probes deliberately differ in mode. pg_monitor keeps 'member',
// the spelling requirements §2.3 pins. pg_read_all_stats probes 'usage',
// because its one job is predicting pg_stat_activity's masking and the server
// has gated that on privilege inheritance since PostgreSQL 15: measured on 15
// and 18, a NOINHERIT member reads true under 'member' while every foreign
// query is <insufficient privilege>. 'usage' matches the gate on 15 through 18
// and can only under-claim on 14, where membership alone suffices - the safe
// direction, since a flag reading false never renders the sentinel as text.
// Do not harmonise them.
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
    (SELECT stats_reset FROM pg_catalog.pg_stat_database WHERE datname = current_database()),
    (SELECT array_agg(name ORDER BY name) FROM s),
    (SELECT array_agg(setting ORDER BY name) FROM s),
    to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL,
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

// logLocationSQL is isolated because it is the one statement that can be denied:
// pg_current_logfile's grant to pg_monitor only landed in PostgreSQL 17, so on
// 14-16 denial is the normal outcome for the recommended role.
const logLocationSQL = `SELECT pg_current_logfile()`

// replicationSQL is separated defensively, though it needs no grant:
// pg_stat_replication masks columns, never rows.
const replicationSQL = `SELECT count(*) FROM pg_catalog.pg_stat_replication`

// serverFactsRow is serverFactsSQL's result, in selection order. Every scalar
// is a pointer, including columns that cannot be NULL today: a non-pointer
// destination turns an unexpected NULL into a scan error and loses the whole
// statement. A NULL array scans into a nil slice natively.
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
	statsReset       *time.Time
	settingNames     []string
	settingValues    []string
	hasCheckpointer  *bool
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
		&r.statsReset,
		&r.settingNames,
		&r.settingValues,
		&r.hasCheckpointer,
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
