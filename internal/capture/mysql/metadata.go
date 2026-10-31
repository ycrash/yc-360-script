package mysql

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// Log access, detected rather than configured: whether this process can open the
// file the server names as its error log. A permission, never a location.
// LogAccessUnknown means the test could not run, and is treated as LogAccessNone.
const (
	LogAccessDirect  = "direct"
	LogAccessNone    = "none"
	LogAccessUnknown = "unknown"
)

// Why the error log cannot be read: log_access_reason.
const (
	logReasonSettingsUnread = "settings_unread"
	logReasonStderr         = "stderr"
	logReasonNoDatadir      = "relative_path_no_datadir"
	logReasonUnreadable     = "unreadable"
)

// Why the open failed, where it did: log_open_error. The two causes an operator
// fixes differently - a path the agent cannot see, a file it may not read - and
// other for the rest.
const (
	logOpenNotFound         = "not_found"
	logOpenPermissionDenied = "permission_denied"
	logOpenOther            = "other"
)

// metadataScope is "cluster": db= means connected through, not about.
const metadataScope = "cluster"

// explainLiteralsRedacted is stated rather than assumed: plans and statement text
// carry the application's values.
const explainLiteralsRedacted = "redacted"

// Metadata is written to my_metadata.txt, one field per row in declaration order.
// Empty means not read. No password field.
type Metadata struct {
	// AgentTS and YC360Version are supplied, not read, so golden tests are deterministic.
	AgentTS      time.Time
	YC360Version string

	TargetID       string
	TargetHost     string
	TargetPort     int
	TargetDatabase string
	TargetUsername string

	// The tls: settings the connection uses. TargetTLSMode is the driver's tls=;
	// TargetTLSCAFile is "system" when the certificate is verified against the
	// system's trust store, and empty when it is not verified.
	TargetTLSMode       string
	TargetTLSEnabled    string
	TargetTLSVerify     string
	TargetTLSCAFile     string
	TargetTLSServerName string

	// ExplainMode and ExplainLiterals are policy, not readings: written in the target
	// block so they survive a refused connection.
	ExplainMode     string
	ExplainLiterals string

	// ConnectError is set by the caller. Non-empty means the file stops here.
	ConnectError string

	// Server facts any account reads.
	Version        string
	VersionComment string
	MySQLVariant   string
	CurrentUser    string
	ConnectionID   string

	// AgentSessionVariables names the variables the agent SETs on each of its
	// sessions. performance_schema.variables_info marks a variable DYNAMIC, with the
	// setting account as SET_USER, after a session-scope SET from any session, so
	// these appear in my_nondefault_settings.txt with the global value unchanged.
	// They are written rather than filtered (spec 1.3: the server excludes the
	// agent's rows): a row naming one of them and set by current_user is the agent's.
	AgentSessionVariables string
	Database              string
	ServerUTC             string
	UptimeSeconds         string

	// Settings, from SHOW GLOBAL VARIABLES; a name the server does not have is empty
	// and listed in SettingsUnavailable.
	Hostname                string
	Port                    string
	Datadir                 string
	Socket                  string
	PidFile                 string
	ServerID                string
	ReadOnly                string
	SuperReadOnly           string
	PerformanceSchema       string
	MaxConnections          string
	LogError                string
	LogErrorServices        string
	LogErrorVerbosity       string
	InnodbPrintAllDeadlocks string
	MaxSQLTextLength        string
	MaxDigestLength         string
	DigestsSize             string
	InformationSchemaExpiry string
	TimeZone                string
	SystemTimeZone          string
	BinlogExpireLogsSeconds string
	SettingsUnavailable     string

	// settingsRead: the settings statement succeeded, so an empty value is the
	// server's and not a failed read.
	settingsRead bool

	// The error log, resolved and tested: LogErrorResolved is the path the agent
	// opened, LogAccess whether it could.
	LogErrorResolved string
	LogAccess        string
	LogAccessReason  string
	LogOpenError     string

	// Capabilities from performance_schema's setup tables.
	HasCPUTimeInstrumentEnabled string
	CPUTimeReason               string
	HasStatementMemoryTracking  string
	HasMemoryInstrumentsEnabled string
	MemoryInstrumentsEnabled    string
	MemoryInstrumentsTotal      string
	StatementsDigestConsumer    string
	CapabilitiesError           string

	// The role's privileges, granted to the account itself; one held through a role
	// is not listed, which EnabledRoles says may be the case.
	HasProcess                 string
	HasReplicationClient       string
	HasPerformanceSchemaSelect string
	EnabledRoles               string
	PrivilegesError            string

	// Tables by engine, among those the role can see: information_schema.TABLES
	// lists only tables the role holds some privilege on.
	TablesVisible     string
	InnodbTables      string
	OtherEngines      string
	InnodbTablesShare string
	TableEnginesError string
	ServerFactsError  string
	SettingsError     string
	UptimeError       string

	// Is the agent on the database's machine? Measured every run, never guessed from
	// the target host. Reason is mandatory whenever the verdict is not yes.
	AgentOnDBHost         string
	AgentOnDBHostBy       string
	AgentOnDBHostEvidence string
	AgentOnDBHostReason   string

	// DeclarationContradicted: agentOnDbHost said yes, the probe measured no. Logged,
	// not written.
	DeclarationContradicted bool

	// HostArtifacts says whether host files were captured or skipped. Follows
	// AgentOnDBHost.
	HostArtifacts string

	// ConnectMS is the dial: TCP, TLS and authentication.
	ConnectMS string
}

// targetFields is what was configured; deliberately no password row.
func targetFields(m Metadata) []dbcore.Field {
	return []dbcore.Field{
		{Key: "agent_ts", Value: dbcore.Timestamp(m.AgentTS)},
		{Key: "yc360_version", Value: m.YC360Version},
		{Key: "target_id", Value: m.TargetID},
		{Key: "target_host", Value: m.TargetHost},
		{Key: "target_port", Value: strconv.Itoa(m.TargetPort)},
		{Key: "target_database", Value: m.TargetDatabase},
		{Key: "target_username", Value: m.TargetUsername},
		{Key: "target_tls_mode", Value: m.TargetTLSMode},
		{Key: "target_tls_enabled", Value: m.TargetTLSEnabled},
		{Key: "target_tls_verify", Value: m.TargetTLSVerify},
		{Key: "target_tls_ca_file", Value: m.TargetTLSCAFile},
		{Key: "target_tls_server_name", Value: m.TargetTLSServerName},

		// Policy, not readings: written here so a refused connection still records what
		// the run intended.
		{Key: "explain_mode", Value: m.ExplainMode},
		{Key: "explain_literals", Value: m.ExplainLiterals},
	}
}

// serverFields is every row requiring a connection, written whether or not the
// statement behind it succeeded.
func serverFields(m Metadata) []dbcore.Field {
	return []dbcore.Field{
		{Key: "version", Value: m.Version},
		{Key: "version_comment", Value: m.VersionComment},
		{Key: "mysql_variant", Value: m.MySQLVariant},
		{Key: "current_user", Value: m.CurrentUser},
		{Key: "connection_id", Value: m.ConnectionID},
		{Key: "agent_session_variables", Value: m.AgentSessionVariables},
		{Key: "database", Value: m.Database},
		{Key: "server_utc", Value: m.ServerUTC},
		{Key: "uptime_seconds", Value: m.UptimeSeconds},

		{Key: "hostname", Value: m.Hostname},
		{Key: "port", Value: m.Port},
		{Key: "datadir", Value: m.Datadir},
		{Key: "socket", Value: m.Socket},
		{Key: "pid_file", Value: m.PidFile},
		{Key: "server_id", Value: m.ServerID},
		{Key: "read_only", Value: m.ReadOnly},
		{Key: "super_read_only", Value: m.SuperReadOnly},
		{Key: "performance_schema", Value: m.PerformanceSchema},
		{Key: "max_connections", Value: m.MaxConnections},
		{Key: "log_error", Value: m.LogError},
		{Key: "log_error_services", Value: m.LogErrorServices},
		{Key: "log_error_verbosity", Value: m.LogErrorVerbosity},
		{Key: "innodb_print_all_deadlocks", Value: m.InnodbPrintAllDeadlocks},
		{Key: "performance_schema_max_sql_text_length", Value: m.MaxSQLTextLength},
		{Key: "performance_schema_max_digest_length", Value: m.MaxDigestLength},
		{Key: "performance_schema_digests_size", Value: m.DigestsSize},
		{Key: "information_schema_stats_expiry", Value: m.InformationSchemaExpiry},
		{Key: "binlog_expire_logs_seconds", Value: m.BinlogExpireLogsSeconds},
		{Key: "time_zone", Value: m.TimeZone},
		{Key: "system_time_zone", Value: m.SystemTimeZone},
		{Key: "settings_unavailable", Value: m.SettingsUnavailable},

		{Key: "log_error_resolved", Value: m.LogErrorResolved},
		{Key: "log_access", Value: m.LogAccess},
		{Key: "log_access_reason", Value: m.LogAccessReason},
		{Key: "log_open_error", Value: m.LogOpenError},

		// The spec's flag, said again as true|false; log_access is its reading.
		{Key: "has_mysql_error_log_access", Value: strconv.FormatBool(m.LogAccess == LogAccessDirect)},

		{Key: "has_cpu_time_instrument_enabled", Value: m.HasCPUTimeInstrumentEnabled},
		{Key: "cpu_time_reason", Value: m.CPUTimeReason},
		{Key: "has_statement_memory_tracking", Value: m.HasStatementMemoryTracking},
		{Key: "has_memory_instruments_enabled", Value: m.HasMemoryInstrumentsEnabled},
		{Key: "memory_instruments_enabled", Value: m.MemoryInstrumentsEnabled},
		{Key: "memory_instruments_total", Value: m.MemoryInstrumentsTotal},
		{Key: "statements_digest_consumer", Value: m.StatementsDigestConsumer},

		{Key: "has_process", Value: m.HasProcess},
		{Key: "has_replication_client", Value: m.HasReplicationClient},
		{Key: "has_performance_schema_select", Value: m.HasPerformanceSchemaSelect},
		{Key: "enabled_roles", Value: m.EnabledRoles},

		{Key: "tables_visible", Value: m.TablesVisible},
		{Key: "innodb_tables", Value: m.InnodbTables},
		{Key: "innodb_tables_share", Value: m.InnodbTablesShare},
		{Key: "other_engines", Value: m.OtherEngines},

		{Key: "agent_on_db_host", Value: m.AgentOnDBHost},
		{Key: "agent_on_db_host_by", Value: m.AgentOnDBHostBy},
		{Key: "agent_on_db_host_evidence", Value: m.AgentOnDBHostEvidence},
		{Key: "agent_on_db_host_reason", Value: m.AgentOnDBHostReason},
		{Key: "host_artifacts", Value: m.HostArtifacts},

		// host_artifacts said again as a true|false flag: whether this capture's host
		// files describe the database's machine.
		{Key: "host_metrics_available", Value: strconv.FormatBool(m.HostArtifacts == dbcore.HostArtifactsCaptured)},

		{Key: "server_facts_error", Value: m.ServerFactsError},
		{Key: "settings_error", Value: m.SettingsError},
		{Key: "uptime_error", Value: m.UptimeError},
		{Key: "capabilities_error", Value: m.CapabilitiesError},
		{Key: "privileges_error", Value: m.PrivilegesError},
		{Key: "table_engines_error", Value: m.TableEnginesError},
		{Key: "connect_ms", Value: m.ConnectMS},
	}
}

// MetadataCollector writes the target block before connecting and the server block
// from the one sample Once() gives it. Held by pointer; Collected() is safe because
// Window.Run is synchronous.
type MetadataCollector struct {
	target       Target
	yc360Version string
	agentNow     time.Time

	declaredOnDBHost bool
	afterCollect     func(Metadata)

	// inspect is the same-host probe's process table; nil is the real one.
	inspect processLister

	collected Metadata
}

// NewMetadata seeds the collector's pre-connection state, which Collected() returns
// if the connection never happens. explainMode is config's own value; "" means the
// key was omitted, which the bundle records as off.
func NewMetadata(t Target, yc360Version string, agentNow time.Time, explainMode string) *MetadataCollector {
	m := &MetadataCollector{target: t, yc360Version: yc360Version, agentNow: agentNow}

	m.collected = targetMetadata(t, agentNow)
	m.collected.YC360Version = yc360Version
	m.collected.ExplainMode = explainModeText(explainMode)
	m.collected.ExplainLiterals = explainLiteralsRedacted

	// Kept by a run whose connection was refused: no server to ask which machine it
	// runs on, so no answer and no host capture - unless the operator declared it.
	m.collected.AgentOnDBHost = dbcore.OnDBHostUnknown
	m.collected.AgentOnDBHostReason = hostReasonNoConnection
	m.collected.HostArtifacts = dbcore.HostArtifactsSkipped
	m.collected.LogAccess = LogAccessUnknown
	m.collected.LogAccessReason = logReasonSettingsUnread

	return m
}

// explainModeText is explain_mode=: the configured mode, or off.
func explainModeText(mode string) string {
	if mode == "" {
		return "off"
	}

	return mode
}

func targetMetadata(t Target, agentNow time.Time) Metadata {
	return Metadata{
		AgentTS:        agentNow,
		TargetID:       t.id(),
		TargetHost:     t.Host,
		TargetPort:     t.Port,
		TargetDatabase: t.Database,
		TargetUsername: t.Username,

		TargetTLSMode:       t.TLS.Mode(),
		TargetTLSEnabled:    strconv.FormatBool(!t.TLS.Disabled),
		TargetTLSVerify:     strconv.FormatBool(!t.TLS.Disabled && t.TLS.Verify),
		TargetTLSCAFile:     t.TLS.caFileText(),
		TargetTLSServerName: t.TLS.ServerName,
	}
}

// DeclareOnDBHost records mysql.agentOnDbHost. It only raises an unknown to yes; a
// measured verdict stands.
func (m *MetadataCollector) DeclareOnDBHost(declared bool) { m.declaredOnDBHost = declared }

// AfterCollect runs once the server block is written, while the window is still
// open, so the caller can act on host_artifacts at its opening edge. It runs on the
// window's goroutine and must return promptly.
func (m *MetadataCollector) AfterCollect(fn func(Metadata)) { m.afterCollect = fn }

// String and GoString redact the password through Target's own.
func (m *MetadataCollector) String() string {
	return fmt.Sprintf("mysql.MetadataCollector{target=%s yc360_version=%s log_access=%s}",
		m.target, m.yc360Version, m.collected.LogAccess)
}

func (m *MetadataCollector) GoString() string { return m.String() }

func (m *MetadataCollector) Artifact() Artifact {
	return Artifact{
		Name:     "my_metadata",
		FileName: "my_metadata.txt",
		Scope:    metadataScope,
		Schedule: dbcore.Once(),

		// Read once, with the other reads of what changes slowly.
		Connection: ConnectionExpensive,
	}
}

// WriteOpening writes what the run was aimed at, knowable before the network and so
// surviving any later failure.
func (m *MetadataCollector) WriteOpening(w io.Writer, s SampleContext) error {
	return writeMetadataBlock(w, "my_metadata_target", location(s), targetFields(m.collected), s.At)
}

// Sample writes what the server said. Collect never errors - each probe records its
// own failure - so only the write can fail.
func (m *MetadataCollector) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	collected := Collect(ctx, q, m.target, m.agentNow, m.inspect)

	// Intent rather than readings, carried across by hand.
	collected.YC360Version = m.yc360Version
	collected.ExplainMode = m.collected.ExplainMode
	collected.ExplainLiterals = m.collected.ExplainLiterals

	// The window owns the dial - the one reading Collect cannot take.
	collected.ConnectMS = dbcore.MillisText(s.ConnectDuration)

	m.collected = collected
	collected = m.ResolveHostDecision()

	header := append(location(s), kv("sample", strconv.Itoa(s.Index)))

	err := writeMetadataBlock(w, "my_metadata_server", header, serverFields(collected), s.At)

	// After the block, so a failed write cannot also lose the host capture.
	if m.afterCollect != nil {
		m.afterCollect(collected)
	}

	return err
}

// Collected is what the run knows: the target alone where it never connected.
func (m *MetadataCollector) Collected() Metadata { return m.collected }

// ResolveHostDecision applies the declaration and settles host_artifacts. Sample
// calls it so the rows it writes are resolved; the caller calls it again for a run
// that never reached the server. Idempotent.
func (m *MetadataCollector) ResolveHostDecision() Metadata {
	if m.declaredOnDBHost {
		switch m.collected.AgentOnDBHost {
		case dbcore.OnDBHostUnknown:
			m.collected.AgentOnDBHost = dbcore.OnDBHostYes
			m.collected.AgentOnDBHostBy = hostConfirmedByConfigured

		case dbcore.OnDBHostNo:
			m.collected.DeclarationContradicted = true
		}
	}

	m.collected.HostArtifacts = dbcore.HostArtifactsSkipped
	if m.collected.AgentOnDBHost == dbcore.OnDBHostYes {
		m.collected.HostArtifacts = dbcore.HostArtifactsCaptured
	}

	return m.collected
}

// writeMetadataBlock renders whole and writes once, so a failure can't leave a
// half-written block.
func writeMetadataBlock(w io.Writer, source string, header []dbcore.HeaderField, fields []dbcore.Field, at time.Time) error {
	var block bytes.Buffer

	if err := dbcore.WriteBlockHeader(&block, engineName, source, dbcore.DefaultArtifactVersion, metadataScope,
		dbcore.FormatCSV, header, at); err != nil {
		return err
	}

	if err := dbcore.WriteKeyValueBody(&block, fields); err != nil {
		return err
	}

	_, err := w.Write(block.Bytes())

	return err
}

// Collect runs the probes; never errors, since each records its own failure. Split
// along the privilege boundary, so one missing grant costs one section.
func Collect(ctx context.Context, q Querier, t Target, agentNow time.Time, inspect processLister) Metadata {
	m := targetMetadata(t, agentNow)
	m.LogAccess = LogAccessUnknown
	m.LogAccessReason = logReasonSettingsUnread

	s := SampleContext{Redact: func(err error) string { return errorText(err, t.Password) }}

	collectServerFacts(ctx, q, s, &m)
	collectSettings(ctx, q, s, &m)
	collectUptime(ctx, q, s, &m)
	collectCapabilities(ctx, q, s, &m)
	collectPrivileges(ctx, q, s, &m)
	collectTableEngines(ctx, q, s, &m)

	// After the settings: both read log_error, datadir and pid_file from them.
	collectLogAccess(&m)
	collectSameHost(&m, inspect, time.Now())

	return m
}

const serverFactsSQL = `SELECT VERSION(), @@version_comment, CURRENT_USER(), CONNECTION_ID(), DATABASE(),
       DATE_FORMAT(UTC_TIMESTAMP(3), '%Y-%m-%dT%H:%i:%s.%fZ')`

// collectServerFacts reads what any account may: who the server is and who the
// agent is to it.
func collectServerFacts(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	row, err := readRow(ctx, q, s, serverFactsSQL)
	if err != nil {
		m.ServerFactsError = s.ErrorText(err)

		return
	}

	m.Version = row[0]
	m.VersionComment = row[1]
	m.MySQLVariant = variantOf(row[0], row[1])
	m.CurrentUser = row[2]
	m.ConnectionID = row[3]
	m.AgentSessionVariables = sessionVariableNames(m.MySQLVariant)
	m.Database = row[4]

	// %f is six digits; the artifact's timestamps carry three.
	if utc := row[5]; len(utc) == len("2006-01-02T15:04:05.000000Z") {
		m.ServerUTC = utc[:len(utc)-4] + "Z"
	} else {
		m.ServerUTC = utc
	}
}

// metadataSettings are the server variables my_metadata.txt records, each with the
// field it fills.
var metadataSettings = []struct {
	name  string
	field func(m *Metadata) *string
}{
	{"hostname", func(m *Metadata) *string { return &m.Hostname }},
	{"port", func(m *Metadata) *string { return &m.Port }},
	{"datadir", func(m *Metadata) *string { return &m.Datadir }},
	{"socket", func(m *Metadata) *string { return &m.Socket }},
	{"pid_file", func(m *Metadata) *string { return &m.PidFile }},
	{"server_id", func(m *Metadata) *string { return &m.ServerID }},
	{"read_only", func(m *Metadata) *string { return &m.ReadOnly }},
	{"super_read_only", func(m *Metadata) *string { return &m.SuperReadOnly }},
	{"performance_schema", func(m *Metadata) *string { return &m.PerformanceSchema }},
	{"max_connections", func(m *Metadata) *string { return &m.MaxConnections }},
	{"log_error", func(m *Metadata) *string { return &m.LogError }},
	{"log_error_services", func(m *Metadata) *string { return &m.LogErrorServices }},
	{"log_error_verbosity", func(m *Metadata) *string { return &m.LogErrorVerbosity }},
	{"innodb_print_all_deadlocks", func(m *Metadata) *string { return &m.InnodbPrintAllDeadlocks }},
	{"performance_schema_max_sql_text_length", func(m *Metadata) *string { return &m.MaxSQLTextLength }},
	{"performance_schema_max_digest_length", func(m *Metadata) *string { return &m.MaxDigestLength }},
	{"performance_schema_digests_size", func(m *Metadata) *string { return &m.DigestsSize }},
	{"information_schema_stats_expiry", func(m *Metadata) *string { return &m.InformationSchemaExpiry }},
	{"binlog_expire_logs_seconds", func(m *Metadata) *string { return &m.BinlogExpireLogsSeconds }},
	{"time_zone", func(m *Metadata) *string { return &m.TimeZone }},
	{"system_time_zone", func(m *Metadata) *string { return &m.SystemTimeZone }},
}

// settingsSQL names every variable it reads: a name the server does not have is
// simply absent from the result, where @@name would fail the whole statement.
var settingsSQL = func() string {
	names := make([]string, len(metadataSettings))
	for i, setting := range metadataSettings {
		names[i] = "'" + setting.name + "'"
	}

	return "SHOW GLOBAL VARIABLES WHERE Variable_name IN (" + strings.Join(names, ", ") + ")"
}()

func collectSettings(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	res, err := read(ctx, q, s, block{query: settingsSQL})
	if err != nil {
		m.SettingsError = s.ErrorText(err)

		return
	}

	values := map[string]string{}
	for _, row := range res.rows {
		if len(row) >= 2 {
			values[strings.ToLower(row[0])] = row[1]
		}
	}

	var unavailable []string

	for _, setting := range metadataSettings {
		value, ok := values[setting.name]
		if !ok {
			unavailable = append(unavailable, setting.name)

			continue
		}

		*setting.field(m) = value
	}

	m.SettingsUnavailable = strings.Join(unavailable, ",")
	m.settingsRead = true
}

func collectUptime(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	res, err := read(ctx, q, s, block{query: "SHOW GLOBAL STATUS WHERE Variable_name = 'Uptime'"})
	if err != nil {
		m.UptimeError = s.ErrorText(err)

		return
	}

	if len(res.rows) == 1 && len(res.rows[0]) >= 2 {
		m.UptimeSeconds = res.rows[0][1]
	}
}

// memoryInstrumentsSQL counts the memory/% instruments less
// memory/performance_schema/%, which are always on, whatever the rest are set to.
// Its _ is LIKE's any character, not an escaped one: with NO_BACKSLASH_ESCAPES in
// the server's sql_mode, '\_' is a backslash and an underscore, and the always-on
// instruments were counted with the rest (508 for 429 on my84). No other
// instrument matches the pattern unescaped.
const memoryInstrumentsSQL = `SELECT COUNT(*) FROM performance_schema.setup_instruments
    WHERE NAME LIKE 'memory/%' AND NAME NOT LIKE 'memory/performance_schema/%'`

// capabilitiesSQL reads performance_schema's switches in one statement. A consumer
// the server does not have - events_statements_cpu before 8.0.28 - reads NULL.
const capabilitiesSQL = `SELECT
  (SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = 'events_statements_cpu'),
  (SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = 'global_instrumentation'),
  (SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = 'thread_instrumentation'),
  (SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = 'statements_digest'),
  (SELECT COUNT(*) FROM performance_schema.setup_instruments
    WHERE NAME LIKE 'statement/%' AND ENABLED = 'YES' AND TIMED = 'YES'),
  (` + memoryInstrumentsSQL + ` AND ENABLED = 'YES'),
  (` + memoryInstrumentsSQL + `)`

// Why has_cpu_time_instrument_enabled is false.
const (
	cpuReasonVersion   = "version_before_8.0.28"
	cpuReasonConsumers = "consumers_off"
	cpuReasonUntimed   = "no_timed_statement_instrument"
	cpuReasonUnread    = "capabilities_unread"

	// cpuReasonPerformanceSchemaOff: the setup tables are empty, so every consumer
	// would read as off; the setting is why.
	cpuReasonPerformanceSchemaOff = reasonPerformanceSchemaOff
)

func collectCapabilities(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	version := parseVersion(m.Version)

	// max_total_memory and max_controlled_memory: 8.0.31. The spec's flag is whether
	// the columns exist. With the memory instruments off they read zero (measured on
	// my80), which my_slow_queries.txt writes as unavailable by
	// has_memory_instruments_enabled's own count.
	if version.known() {
		m.HasStatementMemoryTracking = strconv.FormatBool(version.AtLeast(8, 0, 31))
	}

	// performance_schema off leaves the setup tables empty: nothing to read, and
	// nothing counted.
	if strings.EqualFold(m.PerformanceSchema, "OFF") {
		m.HasCPUTimeInstrumentEnabled = "false"
		m.CPUTimeReason = cpuReasonPerformanceSchemaOff
		m.HasMemoryInstrumentsEnabled = "false"

		if version.known() && !version.AtLeast(8, 0, 28) {
			m.CPUTimeReason = cpuReasonVersion
		}

		return
	}

	row, err := readRow(ctx, q, s, capabilitiesSQL)
	if err != nil {
		m.CapabilitiesError = s.ErrorText(err)
		m.CPUTimeReason = cpuReasonUnread

		if version.known() && !version.AtLeast(8, 0, 28) {
			m.HasCPUTimeInstrumentEnabled = "false"
			m.CPUTimeReason = cpuReasonVersion
		}

		return
	}

	enabled := func(v string) bool { return strings.EqualFold(v, "YES") }

	m.StatementsDigestConsumer = strings.ToLower(row[3])
	m.MemoryInstrumentsEnabled = row[5]
	m.MemoryInstrumentsTotal = row[6]

	memoryOn, _ := strconv.ParseInt(row[5], 10, 64)
	m.HasMemoryInstrumentsEnabled = strconv.FormatBool(memoryOn > 0)

	timed, _ := strconv.ParseInt(row[4], 10, 64)

	// sum_cpu_time is 8.0.28, and needs its consumer, both instrumentation
	// consumers and a timed statement instrument - checked, never switched on.
	switch {
	case version.known() && !version.AtLeast(8, 0, 28):
		m.HasCPUTimeInstrumentEnabled = "false"
		m.CPUTimeReason = cpuReasonVersion

	case !enabled(row[0]) || !enabled(row[1]) || !enabled(row[2]):
		m.HasCPUTimeInstrumentEnabled = "false"
		m.CPUTimeReason = cpuReasonConsumers

	case timed == 0:
		m.HasCPUTimeInstrumentEnabled = "false"
		m.CPUTimeReason = cpuReasonUntimed

	default:
		m.HasCPUTimeInstrumentEnabled = "true"
	}
}

// grantee is CURRENT_USER() in the quoted form the privilege tables name it by.
const grantee = `CONCAT('''', SUBSTRING_INDEX(CURRENT_USER(), '@', 1), '''@''', SUBSTRING_INDEX(CURRENT_USER(), '@', -1), '''')`

// privilegesSQL lists the account's own global privileges, and its SELECT on
// performance_schema as a schema privilege. Filtered to the account: a superuser
// sees every account's rows.
const privilegesSQL = `SELECT PRIVILEGE_TYPE FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = ` + grantee + `
UNION ALL
SELECT CONCAT('performance_schema:', PRIVILEGE_TYPE) FROM information_schema.SCHEMA_PRIVILEGES
 WHERE GRANTEE = ` + grantee + ` AND TABLE_SCHEMA = 'performance_schema'
UNION ALL
SELECT CONCAT('role:', ROLE_NAME, '@', ROLE_HOST) FROM information_schema.ENABLED_ROLES`

func collectPrivileges(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	res, err := read(ctx, q, s, block{query: privilegesSQL})
	if err != nil {
		m.PrivilegesError = s.ErrorText(err)

		return
	}

	held := map[string]bool{}

	var roles []string

	for _, row := range res.rows {
		if role, ok := strings.CutPrefix(row[0], "role:"); ok {
			roles = append(roles, role)

			continue
		}

		held[strings.ToUpper(row[0])] = true
	}

	m.HasProcess = strconv.FormatBool(held["PROCESS"])
	m.HasReplicationClient = strconv.FormatBool(held["REPLICATION CLIENT"] || held["SUPER"])
	m.HasPerformanceSchemaSelect = strconv.FormatBool(held["SELECT"] || held["PERFORMANCE_SCHEMA:SELECT"])
	m.EnabledRoles = strings.Join(roles, ",")
}

// tableEnginesSQL counts the base tables outside the system schemas by engine:
// every InnoDB-specific read in the MySQL section is blind to the rest.
const tableEnginesSQL = `SELECT ENGINE, COUNT(*) FROM information_schema.TABLES
WHERE TABLE_TYPE = 'BASE TABLE'
  AND TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
GROUP BY ENGINE ORDER BY ENGINE`

func collectTableEngines(ctx context.Context, q Querier, s SampleContext, m *Metadata) {
	res, err := read(ctx, q, s, block{query: tableEnginesSQL})
	if err != nil {
		m.TableEnginesError = s.ErrorText(err)

		return
	}

	var total, innodb int64

	var others []string

	for _, row := range res.rows {
		count, _ := strconv.ParseInt(row[1], 10, 64)
		total += count

		if strings.EqualFold(row[0], "InnoDB") {
			innodb += count

			continue
		}

		engine := row[0]
		if engine == "" {
			engine = "none"
		}

		others = append(others, engine+":"+row[1])
	}

	sort.Strings(others)

	m.TablesVisible = strconv.FormatInt(total, 10)
	m.InnodbTables = strconv.FormatInt(innodb, 10)
	m.OtherEngines = strings.Join(others, ",")

	if total > 0 {
		m.InnodbTablesShare = strconv.FormatFloat(float64(innodb)/float64(total), 'f', 3, 64)
	}
}

// collectLogAccess resolves @@log_error as the server would and tests whether this
// process can open it: the same string, read in the agent's own namespace. stderr
// means the server writes no file - the official container image's default.
func collectLogAccess(m *Metadata) {
	if !m.settingsRead {
		return
	}

	resolved, reason := resolveErrorLog(m.LogError, m.Datadir)

	m.LogErrorResolved = resolved

	if resolved == "" {
		m.LogAccess = LogAccessNone
		m.LogAccessReason = reason

		return
	}

	if err := openable(resolved); err != nil {
		m.LogAccess = LogAccessNone
		m.LogAccessReason = logReasonUnreadable
		m.LogOpenError = openErrorClass(err)

		return
	}

	m.LogAccess = LogAccessDirect
	m.LogAccessReason = ""
}

// resolveErrorLog turns @@log_error into a path: absolute as it is, relative against
// the data directory. Empty, with a reason, where the server writes no file.
func resolveErrorLog(logError, datadir string) (string, string) {
	switch {
	case logError == "" || strings.EqualFold(logError, "stderr"):
		return "", logReasonStderr

	case isAbsolutePath(logError):
		return logError, ""

	case datadir == "":
		return "", logReasonNoDatadir
	}

	// A POSIX server's paths stay POSIX whatever the agent's platform.
	if strings.HasPrefix(datadir, "/") {
		return path.Join(datadir, logError), ""
	}

	return filepath.Join(datadir, logError), ""
}

// isAbsolutePath also counts a leading slash, since the agent can be Windows
// talking to a POSIX server.
func isAbsolutePath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

// openable opens rather than stats: permission, not existence, is the question.
func openable(p string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}

	return f.Close()
}

func openErrorClass(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return logOpenNotFound

	case errors.Is(err, fs.ErrPermission):
		return logOpenPermissionDenied
	}

	return logOpenOther
}
