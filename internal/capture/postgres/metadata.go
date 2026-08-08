package postgres

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The capture modes, detected rather than configured: where the agent is
// installed decides, and the agent's job is to report which one it got.
const (
	// ModeDBHost: the server's current log file is readable by this process, so
	// the log-derived artifacts are available.
	ModeDBHost = "pg-dbhost"

	// ModeRemote: it is not - logging_collector is off, or the agent is not on
	// the database host.
	ModeRemote = "pg-remote"

	// ModeUnknown: detection could not run. The server treats it like
	// ModeRemote.
	ModeUnknown = "unknown"
)

// Querier is the seam that makes every statement in this package testable
// without a live server.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RowQuerier is Querier widened to row sets, for the artifacts that read more
// than one row.
type RowQuerier interface {
	Querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Metadata is what a capture found, written to pg_metadata.txt one field per
// row in declaration order.
//
// Every server-derived field holds the value exactly as the server rendered it,
// and an empty string means it was not read. That is the artifact's contract:
// once a connection exists every key is written, with the *Error fields saying
// why a value is empty, so a reader never has to guess whether a missing key
// means an old agent or a failed query.
//
// Nothing here is derived - the arithmetic is left to the server. There is
// deliberately no password field.
type Metadata struct {
	// Known from configuration. AgentTS and YC360Version are supplied by the
	// caller rather than read here, so the golden tests are deterministic.
	AgentTS        time.Time
	YC360Version   string
	TargetHost     string
	TargetPort     int
	TargetDatabase string
	TargetUsername string
	TargetSSLMode  string

	// ConnectError is set by the caller, the only thing that knows about it. A
	// non-empty value tells a reader the file stops here.
	CaptureMode  string
	ConnectError string

	// From serverFactsSQL: identity and run facts.
	CurrentDatabase     string
	CurrentUser         string
	BackendPID          string
	InetServerAddr      string
	InetServerPort      string
	IsInRecovery        string
	PostmasterStartTime string
	StatsReset          string
	Version             string
	ServerVersionNum    string

	// From serverFactsSQL: the pg_settings catalogue. A setting the role may not
	// see, or that this version lacks, is written empty and named in
	// SettingsUnavailable - which distinguishes "no libraries configured" from
	// "not visible to this role".
	MaxConnections          string
	LoggingCollector        string
	LogDestination          string
	LogDirectory            string
	LogFilename             string
	LogLinePrefix           string
	LogMinDurationStatement string
	LogParameterMaxLength   string
	SharedPreloadLibraries  string
	SettingsUnavailable     string

	// The evidence behind CaptureMode, so the conclusion is reproducible from
	// the file rather than asserted by it. DataDirectory arrives with the
	// settings catalogue so a denied pg_current_logfile() cannot cost the row a
	// relative logfile resolves against.
	DataDirectory          string
	CurrentLogfile         string
	CurrentLogfileResolved string
	CurrentLogfileReadable string
	CurrentLogfileError    string

	// From serverFactsSQL: the capability probes every later artifact's query
	// selection reads.
	HasPgMonitorRole        string
	HasPgStatStatements     string
	PgStatStatementsVersion string
	HasPgStatCheckpointer   string
	HasSessionFatalStats    string
	ComputeQueryID          string

	// From replicationSQL.
	ReplicationConfigured string
	ReplicationProbeError string

	// From serverFactsSQL: its failure, and the clock reads it carries.
	QueryError           string
	ServerNow            string
	ServerClockTimestamp string
	AgentTSAtClockRead   time.Time
}

// Collect runs the three statements and resolves the capture mode. It never
// returns an error: a failed probe records its failure in the struct and the
// artifact is still written. The statements are split along the privilege
// boundary so one missing grant costs one section rather than everything.
//
// agentNow is the clock read that pairs with ServerNow, passed in so the golden
// tests are deterministic.
func Collect(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata {
	m := Metadata{
		AgentTS:            agentNow,
		AgentTSAtClockRead: agentNow,
		TargetHost:         t.Host,
		TargetPort:         t.Port,
		TargetDatabase:     t.Database,
		TargetUsername:     t.Username,
		TargetSSLMode:      t.SSLMode,

		// collectLogLocation overwrites this on any path that reaches a
		// conclusion, so an early return cannot look like a determined mode.
		CaptureMode: ModeUnknown,
	}

	collectServerFacts(ctx, q, &m, t.Password)

	// After collectServerFacts on purpose: mode resolution reads the
	// data_directory setting it collected.
	collectLogLocation(ctx, q, &m, t.Password)

	collectReplication(ctx, q, &m, t.Password)

	return m
}

// collectServerFacts runs serverFactsSQL. Its failure is recorded in QueryError
// and leaves every field it would have filled empty - a degradation, not a
// loss: the target block and the capture mode survive.
func collectServerFacts(ctx context.Context, q Querier, m *Metadata, password string) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var row serverFactsRow
	if err := q.QueryRow(ctx, serverFactsSQL, settingNames()).Scan(row.dest()...); err != nil {
		m.QueryError = errorText(err, password)
		return
	}

	m.CurrentDatabase = text(row.currentDatabase)
	m.CurrentUser = text(row.currentUser)
	m.BackendPID = int32Text(row.backendPID)
	m.InetServerAddr = text(row.inetServerAddr)
	m.InetServerPort = int32Text(row.inetServerPort)
	m.IsInRecovery = boolText(row.isInRecovery)
	m.PostmasterStartTime = timeText(row.postmasterStart)
	m.StatsReset = timeText(row.statsReset)
	m.Version = text(row.version)
	m.ServerVersionNum = text(row.serverVersionNum)

	applySettings(m, row.settings())

	m.HasPgMonitorRole = boolText(row.hasPgMonitorRole)
	m.PgStatStatementsVersion = text(row.pgStatStatements)
	m.HasPgStatStatements = strconv.FormatBool(m.PgStatStatementsVersion != "")
	m.HasPgStatCheckpointer = boolText(row.hasCheckpointer)
	m.HasSessionFatalStats = boolText(row.hasSessionFatal)

	m.ServerNow = timeText(row.serverNow)
	m.ServerClockTimestamp = timeText(row.serverClock)
}

// applySettings writes each returned setting into its field and names the rest
// in SettingsUnavailable. A name is missing either because the role may not see
// it or because this version lacks it; the artifact records the fact and leaves
// the judgement to a reader, who has server_version_num and has_pg_monitor_role
// in the same file.
func applySettings(m *Metadata, settings map[string]string) {
	var unavailable []string

	for _, s := range capturedSettings {
		value, ok := settings[s.name]
		if !ok {
			unavailable = append(unavailable, s.name)
			continue
		}

		field := s.field(m)
		*field = value
	}

	m.SettingsUnavailable = strings.Join(unavailable, ",")
}

// collectLogLocation runs logLocationSQL and resolves the capture mode.
//
// The mode predicts "can this process read the server's log file", so that is
// what is tested rather than inferred from the configured host. A relative
// logfile resolves against m.DataDirectory, which is why this runs after
// collectServerFacts.
func collectLogLocation(ctx context.Context, q Querier, m *Metadata, password string) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var logfile *string
	if err := q.QueryRow(ctx, logLocationSQL).Scan(&logfile); err != nil {
		// Unknown rather than guessed: a denial says nothing about where the
		// agent runs, and on 14-16 it is the normal outcome for pg_monitor.
		m.CurrentLogfileError = errorText(err, password)
		return
	}

	m.CurrentLogfile = text(logfile)

	if m.CurrentLogfile == "" {
		// logging_collector is off. An agent genuinely on the database host is
		// recorded as remote, which is harmless: the log-derived artifacts are
		// unavailable either way.
		m.CaptureMode = ModeRemote
		return
	}

	resolved, ok := resolveLogfile(m.CurrentLogfile, m.DataDirectory)
	if !ok {
		m.CaptureMode = ModeRemote
		return
	}

	m.CurrentLogfileResolved = resolved
	m.CurrentLogfileReadable = strconv.FormatBool(isReadable(resolved))

	if m.CurrentLogfileReadable == "true" {
		m.CaptureMode = ModeDBHost
		return
	}

	m.CaptureMode = ModeRemote
}

// resolveLogfile turns pg_current_logfile's answer into a path on this host,
// resolving a relative one against the data directory. It reports false when
// there is nothing to resolve against.
func resolveLogfile(logfile, dataDirectory string) (string, bool) {
	if isAbsolutePath(logfile) {
		return logfile, true
	}

	if dataDirectory == "" {
		return "", false
	}

	return filepath.Join(dataDirectory, logfile), true
}

// isAbsolutePath reports whether the server's path is absolute. A leading slash
// counts even where filepath would not say so: the agent can be a Windows host
// talking to a POSIX server.
func isAbsolutePath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

// isReadable reports whether this process can open the path for reading. Opened
// rather than stat'd because permission, not existence, is the question.
func isReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()

	return true
}

// collectReplication runs replicationSQL. A reader must check is_in_recovery
// before trusting the answer: on a standby pg_stat_replication is legitimately
// empty, so false there is expected topology rather than a finding.
func collectReplication(ctx context.Context, q Querier, m *Metadata, password string) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var count *int64
	if err := q.QueryRow(ctx, replicationSQL).Scan(&count); err != nil {
		m.ReplicationProbeError = errorText(err, password)
		return
	}

	if count == nil {
		return
	}

	m.ReplicationConfigured = strconv.FormatBool(*count > 0)
}

// errorText renders err for an artifact field. The redaction is defence in
// depth - the password is in no statement and no argument today. The flattening
// keeps the struct itself free of multi-line values, so a caller logging one
// gets one line.
func errorText(err error, password string) string {
	if err == nil {
		return ""
	}

	msg := singleLine(err.Error())

	if password != "" {
		msg = strings.ReplaceAll(msg, password, "<redacted>")
	}

	return msg
}
