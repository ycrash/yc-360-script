package postgres

import (
	"strconv"
	"time"
)

// The capture runs against server versions 14-18 and, by design, as a role that
// may hold nothing beyond CONNECT. Every statement here obeys two rules.
//
// Version safety: a capability is established by asking the catalog, never by
// comparing server_version_num against a number the agent carries. pg_upgrade
// carries an old extension schema forward until someone runs ALTER EXTENSION
// ... UPDATE, so a PG15 server can legitimately expose pg_stat_statements'
// pre-1.8 column set.
//
// Privilege safety: settings are read from pg_settings, never with SHOW or
// current_setting(). The view returns no row for an unknown name where SHOW
// raises, and omits a GUC_SUPERUSER_ONLY setting for a role without
// pg_read_all_settings where current_setting(name, true) suppresses only the
// unknown-name error.
//
// The consequence is that serverFactsSQL cannot fail for want of a grant;
// logLocationSQL genuinely can, which is why it is separate.

// timestampLayout is the artifact's timestamp form: RFC 3339 in UTC, to
// milliseconds.
const timestampLayout = "2006-01-02T15:04:05.000Z"

// capturedSettings is the pg_settings catalogue serverFactsSQL reads, in the
// order the artifact writes it. Name and destination field are paired here so
// the list sent to the server, the assignment and the settings_unavailable
// roll-up cannot drift apart.
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
	{"log_min_duration_statement", func(m *Metadata) *string { return &m.LogMinDurationStatement }},
	{"log_parameter_max_length", func(m *Metadata) *string { return &m.LogParameterMaxLength }},
	// superuser-only
	{"shared_preload_libraries", func(m *Metadata) *string { return &m.SharedPreloadLibraries }},
	{"compute_query_id", func(m *Metadata) *string { return &m.ComputeQueryID }},
	// superuser-only. Moved out of logLocationSQL: on 14-16 pg_monitor is denied
	// pg_current_logfile() and this row went down with the statement, even
	// though pg_read_all_settings may see it.
	{"data_directory", func(m *Metadata) *string { return &m.DataDirectory }},
}

// settingNames is the $1 argument serverFactsSQL is given.
func settingNames() []string {
	names := make([]string, len(capturedSettings))
	for i, s := range capturedSettings {
		names[i] = s.name
	}

	return names
}

// serverFactsSQL is the unprivileged statement: identity, run facts, the
// settings catalogue and every capability probe. No path in it can raise for a
// role holding only CONNECT.
//
// Values are recorded in pg_settings.setting form - internal units, so `500`
// rather than SHOW's `500ms`.
//
// The two array_agg subqueries return parallel name and value arrays rather
// than one 2-D array: no multi-dimensional scan support needed, and text arrays
// (unlike a json aggregate) pass bytes through without a UTF-8 conversion that
// could fail on a SQL_ASCII cluster whose log_line_prefix is not valid UTF-8.
//
// inet_server_addr() is unwrapped with host() rather than cast to text: the
// cast renders `172.17.0.2/32`, and the /32 is an artifact of the return type.
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
    now(),
    clock_timestamp()`

// logLocationSQL is isolated because it is the one statement that can be
// denied: pg_current_logfile has EXECUTE revoked from PUBLIC and the grant to
// pg_monitor only landed in PostgreSQL 17, so on 14-16 the denial is the normal
// outcome for the recommended role. It reads nothing else for the same reason.
const logLocationSQL = `SELECT pg_current_logfile()`

// replicationSQL is separated defensively rather than because it is known to
// need a grant: pg_stat_replication masks columns, not rows.
const replicationSQL = `SELECT count(*) FROM pg_catalog.pg_stat_replication`

// serverFactsRow is serverFactsSQL's result, one field per column in selection
// order. Every field is a pointer, including columns that cannot be NULL today:
// a non-pointer destination turns an unexpected NULL into a scan error and
// loses the whole statement. The arrays are exempt - a NULL array scans into a
// nil slice natively.
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
	serverNow        *time.Time
	serverClock      *time.Time
}

// dest returns the scan destinations for serverFactsSQL, in selection order.
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
		&r.serverNow,
		&r.serverClock,
	}
}

// settings pairs the two aggregate columns back into a name/value map. They
// agree by construction; the length guard makes a future edit that breaks that
// produce missing settings rather than mismatched ones.
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

// text renders a nullable text column. NULL and empty are recorded identically:
// an empty value means "not read", and the *_error keys say why.
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

// timeText renders a nullable timestamp in the artifact's form: UTC, as read.
// Nothing here subtracts one timestamp from another - that is the server's
// arithmetic to do.
func timeText(v *time.Time) string {
	if v == nil {
		return ""
	}

	return timestamp(*v)
}
