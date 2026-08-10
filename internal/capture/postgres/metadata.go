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

// The capture modes, detected rather than configured. ModeDBHost means the
// server's current log file is readable by this process, so the log-derived
// artifacts are available; ModeUnknown means detection could not run, and the
// server treats it like ModeRemote.
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
// Every server-derived field holds the value as the server rendered it, and an
// empty string means it was not read. Once a connection exists every key is
// written, with the *Error fields saying why one is empty, so a missing key means
// an old agent rather than a failed query. There is deliberately no password.
type Metadata struct {
	// Known from configuration; AgentTS and YC360Version are supplied so the
	// golden tests are deterministic.
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

	// A setting the role may not see, or that this version lacks, is written
	// empty and named in SettingsUnavailable - which distinguishes "no libraries
	// configured" from "not visible to this role".
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

	// The evidence behind CaptureMode. DataDirectory arrives with the settings
	// catalogue so a denied pg_current_logfile() cannot cost the row a relative
	// logfile resolves against.
	DataDirectory          string
	CurrentLogfile         string
	CurrentLogfileResolved string
	CurrentLogfileReadable string
	CurrentLogfileError    string

	HasPgMonitorRole        string
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

// MetadataCollector writes the target block before the connection exists and the
// server block from the one sample its Once() schedule gives it.
//
// The package's only stateful collector, and so the only one held by pointer:
// the run log's line for this artifact is a reading rather than a sample count.
// Collected() is safe because Window.Run is synchronous.
type MetadataCollector struct {
	target       Target
	yc360Version string
	agentNow     time.Time

	// collect is a seam, nil in production: the readable log file mode detection
	// needs cannot be faked through a Querier.
	collect func(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata

	collected Metadata
}

// NewMetadata builds the collector from what is knowable before the connection,
// which is what Collected() reports if the connection never happens.
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

		// Unknown until collectLogLocation says otherwise, which is the truth
		// about a run whose connection was refused.
		CaptureMode: ModeUnknown,
	}

	return m
}

// String and GoString redact the password. Target's own pair cannot: fmt reaches
// a nested String method only through an exported field, and target is not one.
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

// WritePrologue writes what the run was aimed at, which is knowable before the
// network and so survives any later failure.
func (m *MetadataCollector) WritePrologue(w io.Writer, s SampleContext) error {
	return writeMetadataBlock(w, "pg_metadata_target", []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
	}, targetFields(m.collected), s.At)
}

// Sample writes what the server said. Collect never returns an error - each
// probe records its own failure in a field - so only the write can fail.
func (m *MetadataCollector) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	collected := m.collectWith(ctx, q)

	// Collect returns a fresh value, so the pre-connection version is carried
	// across by hand.
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

// writeMetadataBlock renders whole and writes once: a write failing between the
// header and the body would leave the window's stub behind a half-written block.
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

// metadataScope: every row is about the cluster, and db=/dbid= mean connected
// through rather than about.
const metadataScope = "cluster"

// Collect runs the three statements and resolves the capture mode. It never
// returns an error: a failed probe records its failure in the struct and the
// artifact is still written. The statements are split along the privilege
// boundary so one missing grant costs one section. agentNow pairs with
// ServerNow.
func Collect(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata {
	m := Metadata{
		AgentTS:            agentNow,
		AgentTSAtClockRead: agentNow,
		TargetHost:         t.Host,
		TargetPort:         t.Port,
		TargetDatabase:     t.Database,
		TargetUsername:     t.Username,
		TargetSSLMode:      t.SSLMode,

		// collectLogLocation overwrites this on any path that concludes, so an
		// early return cannot look like a determined mode.
		CaptureMode: ModeUnknown,
	}

	collectServerFacts(ctx, q, &m, t.Password)

	// After collectServerFacts on purpose: mode resolution reads the
	// data_directory setting it collected.
	collectLogLocation(ctx, q, &m, t.Password)

	collectReplication(ctx, q, &m, t.Password)

	return m
}

// collectServerFacts records its failure in QueryError and leaves the fields it
// would have filled empty; the target block and the capture mode survive.
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

// applySettings writes each returned setting into its field and names the rest in
// SettingsUnavailable - missing either because the role may not see it or because
// this version lacks it, which a reader tells apart from the same file.
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

// collectLogLocation resolves the capture mode. The mode predicts "can this
// process read the server's log file", so that is tested rather than inferred
// from the configured host. A relative logfile resolves against m.DataDirectory,
// which is why this runs after collectServerFacts.
func collectLogLocation(ctx context.Context, q Querier, m *Metadata, password string) {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var logfile *string
	if err := q.QueryRow(ctx, logLocationSQL).Scan(&logfile); err != nil {
		// Left unknown: a denial says nothing about where the agent runs, and on
		// 14-16 it is the normal outcome for pg_monitor.
		m.CurrentLogfileError = errorText(err, password)
		return
	}

	m.CurrentLogfile = text(logfile)

	if m.CurrentLogfile == "" {
		// logging_collector is off, so an agent genuinely on the database host is
		// recorded as remote - harmless, the log artifacts are gone either way.
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

// resolveLogfile turns pg_current_logfile's answer into a path on this host. It
// reports false when a relative path has nothing to resolve against.
func resolveLogfile(logfile, dataDirectory string) (string, bool) {
	if isAbsolutePath(logfile) {
		return logfile, true
	}

	if dataDirectory == "" {
		return "", false
	}

	return filepath.Join(dataDirectory, logfile), true
}

// isAbsolutePath counts a leading slash even where filepath would not: the agent
// can be a Windows host talking to a POSIX server.
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

// collectReplication counts connected WAL senders only, so false has two
// readings: a standby, where pg_stat_replication is legitimately empty, and a
// cluster whose only replication is an abandoned slot retaining WAL. Read it
// with is_in_recovery, and treat pg_replication.txt as authoritative for whether
// replication exists at all.
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

// errorText renders err for an artifact field. Redaction is defence in depth -
// the password is in no statement and no argument today.
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
