package postgres

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Capture modes, detected rather than configured. ModeUnknown is treated as ModeRemote.
const (
	ModeDBHost  = "pg-dbhost"
	ModeRemote  = "pg-remote"
	ModeUnknown = "unknown"
)

// Querier is the seam that makes every statement testable without a server.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RowQuerier is Querier widened to row sets.
type RowQuerier interface {
	Querier
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Metadata is written to pg_metadata.txt one field per row in declaration order.
// Empty string means not read; a missing key (vs. empty) means an old agent, not a failed query.
// No password field.
type Metadata struct {
	// AgentTS and YC360Version are supplied, not read, so golden tests are deterministic.
	AgentTS        time.Time
	YC360Version   string
	TargetHost     string
	TargetPort     int
	TargetDatabase string
	TargetUsername string
	TargetSSLMode  string

	// ConnectError is set by the caller. Non-empty means the file stops here.
	CaptureMode  string
	ConnectError string

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

	// Unavailable settings (no permission, or not in this version) are empty and named in
	// SettingsUnavailable.
	MaxConnections          string
	LoggingCollector        string
	LogDestination          string
	LogDirectory            string
	LogFilename             string
	LogLinePrefix           string
	LogRotationAge          string
	LogRotationSize         string
	LogTimezone             string
	LogMinMessages          string
	LogErrorVerbosity       string
	LogMinErrorStatement    string
	LogFileMode             string
	LogMinDurationStatement string
	LogParameterMaxLength   string
	TrackActivityQuerySize  string

	// Effective values behind pg_slow_queries.txt's zeros. PgStatStatementsMax is postmaster-scoped
	// (cluster truth); the other four can be session-overridden, and empty when not preloaded.
	TrackIOTiming                 string
	PgStatStatementsMax           string
	PgStatStatementsTrack         string
	PgStatStatementsTrackPlanning string
	PgStatStatementsTrackUtility  string

	SharedPreloadLibraries string
	SettingsUnavailable    string

	// Evidence behind CaptureMode. DataDirectory comes from the settings catalogue, so a denied
	// pg_current_logfile() still leaves a relative logfile resolvable.
	DataDirectory          string
	CurrentLogfile         string
	CurrentLogfileResolved string
	CurrentLogfileReadable string
	CurrentLogfileError    string

	// Shared with pg_deadlocks.txt/pg_timeouts.txt: all three run the same log resolution, so they
	// can disagree about a moment (rotation, reload) but never about the method.
	LogResolvedBy string
	LogFormats    string

	HasPgMonitorRole string

	// Probed with 'usage' not 'member': matches PG15-18's privilege-inheritance gate
	// (has_privs_of_role); under-claims safely on PG14. HasPgMonitorRole uses 'member'.
	HasPgReadAllStats string

	HasPgStatStatements     string
	PgStatStatementsVersion string
	HasPgStatCheckpointer   string
	HasSessionFatalStats    string
	ComputeQueryID          string

	ReplicationConfigured string
	ReplicationProbeError string

	QueryError           string
	ServerNow            string
	ServerClockTimestamp string
	AgentTSAtClockRead   time.Time
}

// MetadataCollector writes the target block before connecting and the server block from the one
// sample Once() gives it. Held by pointer; Collected() is safe because Window.Run is synchronous.
type MetadataCollector struct {
	target       Target
	yc360Version string
	agentNow     time.Time

	// collect is a test seam; nil in production, since mode detection can't be faked through a Querier.
	collect func(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata

	collected Metadata
}

// NewMetadata seeds the collector's pre-connection state, which Collected() returns if the
// connection never happens.
func NewMetadata(t Target, yc360Version string, agentNow time.Time) *MetadataCollector {
	m := &MetadataCollector{
		target:       t,
		yc360Version: yc360Version,
		agentNow:     agentNow,
	}

	m.collected = Metadata{
		AgentTS:            agentNow,
		AgentTSAtClockRead: agentNow,
		YC360Version:       yc360Version,
		TargetHost:         t.Host,
		TargetPort:         t.Port,
		TargetDatabase:     t.Database,
		TargetUsername:     t.Username,
		TargetSSLMode:      t.SSLMode,

		// Unknown until collectLogLocation says otherwise; true for a run whose connection was refused.
		CaptureMode: ModeUnknown,
	}

	return m
}

// String and GoString redact the password; Target's own String/GoString can't, since fmt only
// reaches a nested String method through an exported field, and target isn't one.
func (m *MetadataCollector) String() string {
	return fmt.Sprintf("postgres.MetadataCollector{target=%s yc360_version=%s capture_mode=%s}",
		m.target, m.yc360Version, m.collected.CaptureMode)
}

func (m *MetadataCollector) GoString() string { return m.String() }

func (m *MetadataCollector) Artifact() Artifact {
	return Artifact{
		Name:     "pg_metadata",
		FileName: "pg_metadata.txt",
		Scope:    metadataScope,
		Schedule: Once(),
	}
}

// WritePrologue writes what the run was aimed at, knowable before the network and so survives any
// later failure.
func (m *MetadataCollector) WritePrologue(w io.Writer, s SampleContext) error {
	return writeMetadataBlock(w, "pg_metadata_target", []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
	}, targetFields(m.collected), s.At)
}

// Sample writes what the server said; Collect never errors (each probe records its own failure),
// so only the write can fail.
func (m *MetadataCollector) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	collected := m.collectWith(ctx, q)

	// Collect returns a fresh value; pre-connection version carried across by hand.
	collected.YC360Version = m.yc360Version

	m.collected = collected

	return writeMetadataBlock(w, "pg_metadata_server", []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}, serverBlockFields(collected), s.At)
}

func (m *MetadataCollector) Collected() Metadata { return m.collected }

func (m *MetadataCollector) collectWith(ctx context.Context, q Querier) Metadata {
	if m.collect != nil {
		return m.collect(ctx, q, m.target, m.agentNow)
	}

	return Collect(ctx, q, m.target, m.agentNow)
}

// writeMetadataBlock renders whole and writes once, so a failure can't leave a half-written block.
func writeMetadataBlock(w io.Writer, source string, header []headerField, fields []field, at time.Time) error {
	var block bytes.Buffer

	if err := writeBlockHeader(&block, source, metadataScope, header, at); err != nil {
		return err
	}

	if err := writeKeyValueBody(&block, fields); err != nil {
		return err
	}

	_, err := w.Write(block.Bytes())

	return err
}

// metadataScope is "cluster": db=/dbid= mean connected through, not about.
const metadataScope = "cluster"

// Collect runs the three statements and resolves the capture mode; never errors, since each probe
// records its own failure. Split along the privilege boundary, so one missing grant costs one section.
func Collect(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata {
	m := Metadata{
		AgentTS:            agentNow,
		AgentTSAtClockRead: agentNow,
		TargetHost:         t.Host,
		TargetPort:         t.Port,
		TargetDatabase:     t.Database,
		TargetUsername:     t.Username,
		TargetSSLMode:      t.SSLMode,

		// collectLogLocation overwrites this on any completed path; an early return can't look like
		// a determined mode.
		CaptureMode: ModeUnknown,
	}

	collectServerFacts(ctx, q, &m, t.Password)

	// Runs after collectServerFacts: mode resolution reads the data_directory setting it collected.
	collectLogLocation(ctx, q, &m, t.Password)

	collectReplication(ctx, q, &m, t.Password)

	return m
}

// collectServerFacts records failure in QueryError and leaves its fields empty; target block and
// capture mode survive.
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
	m.HasPgReadAllStats = boolText(row.hasPgReadAllStat)
	m.PgStatStatementsVersion = text(row.pgStatStatements)
	m.HasPgStatStatements = strconv.FormatBool(m.PgStatStatementsVersion != "")
	m.HasPgStatCheckpointer = boolText(row.hasCheckpointer)
	m.HasSessionFatalStats = boolText(row.hasSessionFatal)

	m.ServerNow = timeText(row.serverNow)
	m.ServerClockTimestamp = timeText(row.serverClock)
}

// applySettings writes each returned setting into its field and names the rest in
// SettingsUnavailable (no permission, or not in this version).
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

// collectLogLocation shares resolveLogSource with pg_deadlocks.txt/pg_timeouts.txt so all three
// agree on method. Mode is tested (log file actually readable), not inferred from the configured host.
func collectLogLocation(ctx context.Context, q Querier, m *Metadata, password string) {
	source := resolveLogSource(ctx, q, logSettingsFromMetadata(m),
		func(err error) string { return errorText(err, password) })

	m.CaptureMode = source.captureMode()
	m.LogResolvedBy = source.resolvedBy
	m.LogFormats = source.formatNames()

	// Last route's error, not the only route's: before disk routes existed, a denied
	// pg_current_logfile() was the whole story.
	m.CurrentLogfileError = source.err

	if source.raw != "" {
		m.CurrentLogfile = source.raw
	}

	if source.path == "" {
		return
	}

	m.CurrentLogfileResolved = source.path
	m.CurrentLogfileReadable = strconv.FormatBool(source.reason != reasonUnreadable)
}

// logSettingsFromMetadata reuses collectServerFacts's read rather than querying settings again.
// read is false when that statement failed, distinguishing "no path found" from "couldn't resolve".
func logSettingsFromMetadata(m *Metadata) logSettings {
	return logSettings{
		dataDirectory:    m.DataDirectory,
		logDirectory:     m.LogDirectory,
		logFilename:      m.LogFilename,
		loggingCollector: m.LoggingCollector,
		logDestination:   m.LogDestination,
		serverAddr:       m.InetServerAddr,
		read:             m.QueryError == "",
	}
}

// resolveLogfile turns pg_current_logfile's answer into a path; false when a relative path has
// nothing to resolve against.
func resolveLogfile(logfile, dataDirectory string) (string, bool) {
	if isAbsolutePath(logfile) {
		return logfile, true
	}

	if dataDirectory == "" {
		return "", false
	}

	return filepath.Join(dataDirectory, logfile), true
}

// isAbsolutePath also counts a leading slash, since the agent can be Windows talking to a POSIX server.
func isAbsolutePath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

// isReadable opens rather than stats: permission, not existence, is the question.
func isReadable(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	f.Close()

	return true
}

// collectReplication counts connected WAL senders only: false means either a standby (legitimately
// empty) or an abandoned slot. Treat pg_replication.txt as authoritative for whether replication exists.
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

// errorText renders err for an artifact field; redaction is defence in depth (the password isn't
// in any statement or argument today).
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
