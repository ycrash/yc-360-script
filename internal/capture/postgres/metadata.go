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

// MetadataCollector writes pg_metadata.txt as one of the window's collectors:
// the target block before the connection exists, the server block from the one
// sample its Once() schedule gives it.
//
// It is the package's only stateful collector, and the only one held by pointer
// where Health{} and Bloat{} are values. Sample keeps what it collected because
// the run log's line for this artifact is a reading - which capture mode the run
// got, and which probes were denied - rather than the sample count every other
// artifact reports, and a count of 1/1 is true and useless. Collected() is that
// read, and it is safe because Window.Run is synchronous.
type MetadataCollector struct {
	target       Target
	yc360Version string
	agentNow     time.Time

	// collect is a seam, nil in production: a golden fixture pins one server's
	// regime without a server in that regime - the readable log file mode
	// detection needs cannot be faked through a Querier.
	collect func(ctx context.Context, q Querier, t Target, agentNow time.Time) Metadata

	collected Metadata
}

// NewMetadata builds the collector from what is knowable before the connection.
// Collect returns a Metadata rather than filling one in, so the target, the
// agent's version and its clock read are held here until it does - and they are
// what Collected() reports if the connection never happens.
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

		// Unknown until collectLogLocation says otherwise, which is also the
		// truth about a run whose connection was refused.
		CaptureMode: ModeUnknown,
	}

	return m
}

// String renders the collector for logs with the password redacted, and
// GoString does the same for %#v.
//
// Target's own pair does not cover this and cannot: fmt reaches a nested String
// method only through an exported field, and target is not one - so a collector
// printed without these would render its Target, and the password in it, as a
// bare struct. That is also why nothing holds a collector in a struct it logs;
// Collected() returns a Metadata, which has no password field at all.
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

// WritePrologue writes the pg_metadata_target block: what the run was aimed at,
// which is knowable before the network and is therefore the block a reader can
// rely on being there whatever happened next.
func (m *MetadataCollector) WritePrologue(w io.Writer, s SampleContext) error {
	return writeMetadataBlock(w, "pg_metadata_target", []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
	}, targetFields(m.collected), s.At)
}

// Sample runs the capture and writes the pg_metadata_server block: what the
// server said, which exists only because a connection did.
//
// Collect never returns an error - each probe records its own failure in a
// field - so the only way this fails is the write, which the window records as
// the artifact's IOErr.
func (m *MetadataCollector) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	collected := m.collectWith(ctx, q)

	// Collect returns a fresh value rather than filling one in, so the version
	// stamped before the connection has to be carried across.
	collected.YC360Version = m.yc360Version

	m.collected = collected

	return writeMetadataBlock(w, "pg_metadata_server", []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}, serverBlockFields(collected), s.At)
}

// Collected is what the last sample read, or what was configured if there was
// never one. The adapter reads it after the window closes to render the run
// log's line for this artifact.
func (m *MetadataCollector) Collected() Metadata { return m.collected }

func (m *MetadataCollector) collectWith(ctx context.Context, q Querier) Metadata {
	if m.collect != nil {
		return m.collect(ctx, q, m.target, m.agentNow)
	}

	return Collect(ctx, q, m.target, m.agentNow)
}

// writeMetadataBlock renders one block whole and writes it once, the Collector
// contract's rule rather than one collector's: a write failing between the
// header and the body would leave the window's stub block behind a half-written
// block.
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

// metadataScope is the artifact's and both of its body blocks': every row is
// about the cluster, and db=/dbid= mean connected through rather than about.
const metadataScope = "cluster"

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
