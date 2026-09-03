package postgres

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The poll is the recurring reading behind M3 database monitoring: one fresh
// connection, one statement, and - only where this machine is confirmed to be the
// database's - the free space on its volumes. Every value is raw; nothing is kept
// between cycles.

// M3FileName is the artifact's name, and m3Source the header's source=, the way
// pg_metadata.txt matches pg_metadata_server. The function that produces it is
// named for the reading it takes, not for the artifact - as Collect produces
// pg_metadata.
const M3FileName = "pg_m3.txt"

const m3Source = "pg_m3"

// m3Scope is "cluster": the connection counts and settings are the server's, not
// the connected database's.
const m3Scope = "cluster"

// Why the heartbeat produced no numbers. The payload carries the token; the full
// text goes to the agent log with the password removed.
const (
	heartbeatTooManyConnections = "too_many_connections"
	heartbeatAuthFailed         = "auth_failed"
	heartbeatTimeout            = "timeout"
	heartbeatUnreachable        = "unreachable"
	heartbeatStatementFailed    = "statement_failed"
	heartbeatOther              = "other"
)

// Why the disk rows are absent. The runner's own disk is never sent in their place.
const (
	diskReasonNoConnection       = "no_connection"
	diskReasonDBHostUnknown      = "db_host_unknown"
	diskReasonNotSameHost        = "not_same_host"
	diskReasonSettingUnavailable = "setting_unavailable"
	diskReasonReadFailed         = "read_failed"
)

// agentCPUInterval is the window agent_cpu_pct is measured over. A cumulative
// figure since process start would flatten to nothing on a long-running agent, and
// keeping the previous reading would be state between cycles.
const agentCPUInterval = 200 * time.Millisecond

// PollRequest is everything one poll needs. Now and Runner are supplied rather
// than read so the payload is testable without a clock or a hostname.
type PollRequest struct {
	Target Target
	Runner string
	Now    time.Time

	// DeclaredOnDBHost is postgres.agentOnDbHost. It raises an unknown verdict to
	// yes and is recorded as such; a measurement always wins.
	DeclaredOnDBHost bool
}

// PollResult is one cycle's reading. An empty field is not written: every value
// that could not be read has a reason row in its place.
type PollResult struct {
	TS     time.Time
	Runner string

	TargetHost     string
	TargetPort     int
	TargetDatabase string

	AgentOnDBHost       string
	AgentOnDBHostBy     string
	AgentOnDBHostReason string

	ServerVersionNum string

	ConnectMS string
	QueryMS   string

	CurrentConnections string
	BackendsTotal      string
	BackendsMasked     string

	MaxConnections               string
	SuperuserReservedConnections string
	ReservedConnections          string

	RunnerLoad1 string
	AgentCPUPct string

	DiskFreeBytes  string
	DiskTotalBytes string
	DiskMount      string

	HeartbeatError string
	DiskReason     string

	// DeclarationContradicted: agentOnDbHost said yes, this poll measured no. The
	// measurement stands; the disagreement is logged, not written.
	DeclarationContradicted bool

	// LogError is the failure in full, for the agent log only. The payload carries
	// the token beside it and never the text.
	LogError string

	// Notes are the partial failures that cost a path rather than the reading: an
	// unreadable volume, a denied tablespace query. Logged, never written.
	Notes []string
}

// Poll takes one reading. It never returns an error: a database that cannot be
// reached is the most important reading of all, and it is reported in the payload
// rather than lost.
func Poll(ctx context.Context, req PollRequest) PollResult {
	out := PollResult{
		TS:             req.Now,
		Runner:         req.Runner,
		TargetHost:     req.Target.Host,
		TargetPort:     req.Target.Port,
		TargetDatabase: req.Target.Database,

		// What a run that never reaches the server keeps: no backend to look for.
		AgentOnDBHost:       OnDBHostUnknown,
		AgentOnDBHostReason: hostReasonNoConnection,
	}

	ctx, cancel := context.WithTimeout(ctx, ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, req.Target)
	if err != nil {
		out.HeartbeatError = classifyHeartbeat(err)

		// errorText, not ConnectErrorText: the latter flattens a classified
		// failure to its bare token because the artifact row must be matchable,
		// and that would cost the log the one thing it is for. A refusal at
		// max_connections, a role's CONNECTION LIMIT and a database's are all
		// SQLSTATE 53300 with three different fixes, told apart only by the text.
		out.LogError = errorText(err, req.Target.Password)

		// The declaration still applies: it exists for exactly this case, and the
		// top capture it authorises is the only host evidence a down database leaves.
		out.applyDeclaration(req.DeclaredOnDBHost)
		out.DiskReason = diskReasonNoConnection
		out.readRunnerHealth()

		return out
	}
	defer conn.Close(ctx)

	out.ConnectMS = millisText(conn.ConnectDuration())
	out.collect(ctx, conn, req)

	return out
}

// collect takes everything on the far side of an open connection, in order: the
// declaration is folded in before the two host-scoped reads, since both are gated
// on the verdict it can raise.
func (p *PollResult) collect(ctx context.Context, q RowQuerier, req PollRequest) {
	row, elapsed, err := readPollRow(ctx, q)
	if err != nil {
		p.HeartbeatError = classifyHeartbeat(err)
		p.LogError = errorText(err, req.Target.Password)

		// The connection opened, so the reason is no longer no_connection: there
		// was simply no backend PID to look for, and no data directory either.
		p.AgentOnDBHostReason = hostReasonBackendPIDUnread
		p.applyDeclaration(req.DeclaredOnDBHost)
		p.readRunnerHealth()
		p.readDisk(ctx, q, "", req.Target.Password)

		return
	}

	p.QueryMS = millisText(elapsed)
	p.applyRow(row)
	p.checkOnDBHost(row, req.Target)
	p.applyDeclaration(req.DeclaredOnDBHost)
	p.readRunnerHealth()
	p.readDisk(ctx, q, text(row.dataDirectory), req.Target.Password)
}

// applyRow copies the statement's answers. Every one is written as the server sent
// it; the server does the arithmetic.
func (p *PollResult) applyRow(row pollRow) {
	p.ServerVersionNum = text(row.serverVersionNum)
	p.CurrentConnections = int64Text(row.currentConnections)
	p.BackendsTotal = int64Text(row.backendsTotal)
	p.BackendsMasked = int64Text(row.backendsMasked)
	p.MaxConnections = text(row.maxConnections)
	p.SuperuserReservedConnections = text(row.superuserReserved)
	p.ReservedConnections = text(row.reservedConnections)
}

// checkOnDBHost runs the same function the deep dive runs, on this poll's own
// connection. There is no memory of the last answer: a fixed deployment reports
// yes on the next cycle with nothing to reset.
func (p *PollResult) checkOnDBHost(row pollRow, t Target) {
	result := checkSameHost(sameHostFacts{
		backendPID:      int32Text(row.backendPID),
		role:            text(row.roleName),
		database:        text(row.databaseName),
		clientAddr:      text(row.clientAddr),
		clientPort:      int32Text(row.clientPort),
		postmasterStart: timeText(row.postmasterStart),
		dialedSocket:    strings.HasPrefix(t.Host, "/"),
	}, newProcessInspector(text(row.updateProcessTitle)))

	p.AgentOnDBHost = result.verdict
	p.AgentOnDBHostBy = result.by
	p.AgentOnDBHostReason = result.reason
}

// applyDeclaration folds postgres.agentOnDbHost in through the same rule the deep
// dive uses, so the two cannot disagree about what a declaration is worth.
func (p *PollResult) applyDeclaration(declared bool) {
	m := Metadata{
		AgentOnDBHost:       p.AgentOnDBHost,
		AgentOnDBHostBy:     p.AgentOnDBHostBy,
		AgentOnDBHostReason: p.AgentOnDBHostReason,
	}

	p.DeclarationContradicted = applyOnDBHostDeclaration(&m, declared)

	p.AgentOnDBHost = m.AgentOnDBHost
	p.AgentOnDBHostBy = m.AgentOnDBHostBy
	p.AgentOnDBHostReason = m.AgentOnDBHostReason
}

// OnDBHost reports whether this poll confirmed the runner is the database host. It
// is the gate on every host-scoped reading in the cycle, the top capture included.
func (p PollResult) OnDBHost() bool { return p.AgentOnDBHost == OnDBHostYes }

// WritePoll renders the payload: one block header and a metric,value body.
func WritePoll(w io.Writer, p PollResult) error {
	if err := writeBlockHeader(w, m3Source, m3Scope, p.headerFields(), p.TS); err != nil {
		return err
	}

	return writeRows(w, []string{"metric", "value"}, p.rows())
}

// headerFields names the runner and the target, so the server can tell two
// runners polling one database apart. No password, no query text.
func (p PollResult) headerFields() []headerField {
	fields := []headerField{
		{"runner", p.Runner},
		{"target_host", p.TargetHost},
		{"target_port", strconv.Itoa(p.TargetPort)},
		{"target_database", p.TargetDatabase},
		{"agent_on_db_host", p.AgentOnDBHost},
	}

	// The two are complementary: which test produced a yes, or why there was none.
	// by also tells a measurement from the operator's declaration.
	if p.AgentOnDBHostBy != "" {
		fields = append(fields, headerField{"agent_on_db_host_by", p.AgentOnDBHostBy})
	}

	// Mandatory whenever the verdict is not yes; a bare no or unknown is a bug.
	if p.AgentOnDBHostReason != "" {
		fields = append(fields, headerField{"agent_on_db_host_reason", p.AgentOnDBHostReason})
	}

	if p.ServerVersionNum != "" {
		fields = append(fields, headerField{"server_version_num", p.ServerVersionNum})
	}

	return fields
}

// rows drops every empty value: a reading that did not happen is never a zero, and
// never absent without the reason row beside it.
func (p PollResult) rows() [][]string {
	var out [][]string

	for _, row := range []field{
		{"connect_ms", p.ConnectMS},
		{"query_ms", p.QueryMS},
		{"heartbeat_error", p.HeartbeatError},
		{"current_connections", p.CurrentConnections},
		{"backends_total", p.BackendsTotal},
		{"backends_masked", p.BackendsMasked},
		{"max_connections", p.MaxConnections},
		{"superuser_reserved_connections", p.SuperuserReservedConnections},
		{"reserved_connections", p.ReservedConnections},
		{"runner_load1", p.RunnerLoad1},
		{"agent_cpu_pct", p.AgentCPUPct},
		{"disk_free_bytes", p.DiskFreeBytes},
		{"disk_total_bytes", p.DiskTotalBytes},
		{"disk_mount", p.DiskMount},
		{"disk_reason", p.DiskReason},
	} {
		if row.value == "" {
			continue
		}

		out = append(out, []string{row.key, row.value})
	}

	return out
}

// ErrorDetail is LogError without the token the caller prints beside it: a
// classified failure carries its own token as a prefix, and printing both reads
// as the token twice.
func (p PollResult) ErrorDetail() string {
	if p.HeartbeatError == "" {
		return p.LogError
	}

	return strings.TrimPrefix(p.LogError, p.HeartbeatError+": ")
}

// LogLine is the one line every reading writes, whatever it found. It names the
// artifact so a support reader can go straight to it.
func (p PollResult) LogLine(sent bool) string {
	line := "pg_m3: target=" + p.TargetHost + ":" + strconv.Itoa(p.TargetPort) + "/" + p.TargetDatabase +
		" agent_on_db_host=" + p.AgentOnDBHost

	if p.AgentOnDBHostReason != "" {
		line += " reason=" + p.AgentOnDBHostReason
	}

	return line + " sent=" + strconv.FormatBool(sent)
}

// pollSQL is the one statement: the heartbeat's second half, the connection
// figures, the settings, and the facts the same-host check needs. Settings come
// from pg_settings, not SHOW, so a name this role may not read comes back NULL
// instead of raising and losing the whole statement.
//
// The aggregates and the session functions share one row: none of the latter
// references a column, so no GROUP BY is needed.
const pollSQL = `SELECT
    pg_backend_pid(),
    current_user::text,
    current_database()::text,
    host(inet_client_addr()),
    inet_client_port(),
    pg_postmaster_start_time(),
    count(*),
    count(*) FILTER (WHERE backend_type = 'client backend' OR backend_type IS NULL),
    count(*) FILTER (WHERE backend_type IS NULL),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'max_connections'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'superuser_reserved_connections'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'reserved_connections'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'server_version_num'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'data_directory'),
    (SELECT setting FROM pg_catalog.pg_settings WHERE name = 'update_process_title')
FROM pg_catalog.pg_stat_activity`

// Every scalar is a pointer so an unexpected NULL cannot fail the statement.
type pollRow struct {
	backendPID      *int32
	roleName        *string
	databaseName    *string
	clientAddr      *string
	clientPort      *int32
	postmasterStart *time.Time

	backendsTotal      *int64
	currentConnections *int64
	backendsMasked     *int64

	maxConnections      *string
	superuserReserved   *string
	reservedConnections *string
	serverVersionNum    *string
	dataDirectory       *string
	updateProcessTitle  *string
}

// dest is in pollSQL's selection order.
func (r *pollRow) dest() []any {
	return []any{
		&r.backendPID,
		&r.roleName,
		&r.databaseName,
		&r.clientAddr,
		&r.clientPort,
		&r.postmasterStart,
		&r.backendsTotal,
		&r.currentConnections,
		&r.backendsMasked,
		&r.maxConnections,
		&r.superuserReserved,
		&r.reservedConnections,
		&r.serverVersionNum,
		&r.dataDirectory,
		&r.updateProcessTitle,
	}
}

// readPollRow returns the row and its round trip, which is query_ms.
func readPollRow(ctx context.Context, q Querier) (pollRow, time.Duration, error) {
	ctx, cancel := statementContext(ctx)
	defer cancel()

	sent := time.Now()

	var row pollRow
	err := q.QueryRow(ctx, pollSQL).Scan(row.dest()...)

	return row, time.Since(sent), err
}

// classifyHeartbeat reduces a failure to one of the six tokens. Order matters:
// a refusal at max_connections arrives as an authentication-stage error, and a
// timeout arrives wrapped in whatever the transport was doing when it expired.
func classifyHeartbeat(err error) string {
	switch {
	case err == nil:
		return ""

	case errors.Is(err, ErrTooManyConnections):
		return heartbeatTooManyConnections

	case isAuthFailure(err):
		return heartbeatAuthFailed

	case isTimeout(err):
		return heartbeatTimeout

	case isUnreachable(err):
		return heartbeatUnreachable

	case sqlState(err) != "":
		// The server answered and refused the statement, which is a different
		// fault from never having reached it.
		return heartbeatStatementFailed
	}

	return heartbeatOther
}

// isAuthFailure matches SQLSTATE class 28, which covers both a rejected password
// and a role that may not connect.
func isAuthFailure(err error) bool {
	return strings.HasPrefix(sqlState(err), "28")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error

	return errors.As(err, &netErr) && netErr.Timeout()
}

// isUnreachable covers the failures that happen before any server answers: name
// resolution, a refused port, a dead route.
func isUnreachable(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}

	var opErr *net.OpError

	return errors.As(err, &opErr)
}

// sqlState returns the first SQLSTATE in err's tree, or "" when the failure never
// reached the server. Walks the tree like hasSQLState: pgx joins one error per
// resolved address, and errors.As would stop at the first.
func sqlState(err error) string {
	if err == nil {
		return ""
	}

	if pgErr, ok := err.(*pgconn.PgError); ok {
		return pgErr.Code
	}

	switch unwrapper := err.(type) {
	case interface{ Unwrap() error }:
		return sqlState(unwrapper.Unwrap())

	case interface{ Unwrap() []error }:
		for _, joined := range unwrapper.Unwrap() {
			if code := sqlState(joined); code != "" {
				return code
			}
		}
	}

	return ""
}
