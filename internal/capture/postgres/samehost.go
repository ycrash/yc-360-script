package postgres

import (
	"context"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Is the agent on the machine running the database? Every error path returns
// unknown or no, never a false yes: a wrong yes would label another machine's
// CPU, memory and kernel readings as the database host's.
const (
	OnDBHostYes     = "yes"
	OnDBHostNo      = "no"
	OnDBHostUnknown = "unknown"
)

// Which test produced a yes. Stored separately so a new test can be added
// without changing the set of rows.
const (
	confirmedByBackendPID      = "backend_pid"
	confirmedByBackendPIDNSpid = "backend_pid_nspid"
	confirmedByPostmasterStart = "postmaster_start_time"

	// A fourth value, "configured", will arrive with the operator declaration that
	// covers a database too far down to answer. It is not named here because
	// nothing would use it yet.
)

// Why the verdict is not yes. Mandatory whenever it is not.
const (
	hostReasonNoConnection     = "no_connection"
	hostReasonBackendPIDUnread = "backend_pid_unread"
	hostReasonContainer        = "container"
	hostReasonProcRestricted   = "proc_restricted"
	hostReasonPlatformNoTitles = "platform_no_titles"
	hostReasonPIDAbsent        = "pid_absent"
	hostReasonTitleMismatch    = "title_mismatch"
	hostReasonManagedService   = "managed_service"
)

// Supporting signals. Any of these can be produced by a tunnel or a pooler from a
// machine that is not the database host, so they are recorded but never used to
// decide the verdict.
const (
	evidenceClientSocket    = "client_socket"
	evidenceLogFile         = "log_file"
	evidenceServerAddrMatch = "server_addr_match"
	evidenceInetServerNull  = "inet_server_null"
)

// postmasterStartTolerance covers integer truncation on both sides. Two readings
// of one postmaster start were measured 1s apart.
const postmasterStartTolerance = 2 * time.Second

// managedServiceSQL gives a certain no: these roles exist only on the managed
// platforms, pg_roles is readable by every role, and the agent cannot run on the
// machine hosting such a database. Works with only LOGIN.
const managedServiceSQL = `SELECT EXISTS (
    SELECT 1 FROM pg_catalog.pg_roles
     WHERE rolname IN ('rds_superuser', 'cloudsqladmin', 'azure_pg_admin', 'alloydbsuperuser')
)`

// backendTitle is what a PostgreSQL backend writes over its own argv:
//
//	postgres: <role> <database> <client_host>(<client_port>) <activity>
//	postgres: <role> <database> [local] <activity>
//
// With update_process_title=off the fixed part above still survives on Linux:
// only the trailing activity stops updating. That is why the match never reads
// the activity.
var backendTitle = regexp.MustCompile(
	`^postgres:\s+(?:\S+:\s+)?(\S+)\s+(\S+)\s+(?:\[local\]|([^\s(]+)\((\d+)\))`)

// parsedTitle is the fixed part of a backend title. local is true for a
// unix-socket connection, which has no address and port to compare.
type parsedTitle struct {
	role     string
	database string
	addr     string
	port     string
	local    bool
}

// parseBackendTitle reports ok=false for anything that is not a PostgreSQL
// backend title, which is the common case for a colliding PID.
func parseBackendTitle(title string) (parsedTitle, bool) {
	m := backendTitle.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil {
		return parsedTitle{}, false
	}

	return parsedTitle{
		role:     m[1],
		database: m[2],
		addr:     m[3],
		port:     m[4],
		local:    m[3] == "",
	}, true
}

// sameHostFacts is what the server reported about this connection. An empty
// string means the query that would have set it did not run.
type sameHostFacts struct {
	backendPID      string
	role            string
	database        string
	clientAddr      string
	clientPort      string
	serverAddr      string
	postmasterStart string

	// logDirect is log_access == direct: the agent opened the server's log file.
	// Strong evidence but not proof - an NFS-mounted log directory is readable
	// from another machine.
	logDirect bool

	// dialedSocket is true when the agent itself dialed a unix socket path.
	dialedSocket bool

	// managedService is the result of managedServiceSQL, which gives a certain no.
	managedService bool
}

// sameHostResult is the probe's answer. reason is empty exactly when verdict is yes.
type sameHostResult struct {
	verdict  string
	by       string
	reason   string
	evidence []string
}

// processInspector reads the local process table. Each platform implements it
// differently: Linux implements every method, platforms with no readable process
// title implement only parentStartTime.
type processInspector interface {
	// titlesReadable is false where the platform has no title to read at all.
	titlesReadable() bool

	// title returns the command line of pid. ok=false means the process is not
	// visible, which has four possible causes the caller must tell apart before
	// answering no.
	title(pid int) (string, bool)

	// canSeeForeignProcesses reports whether this agent can see processes it does
	// not own. Under hidepid it cannot, so a missing process means nothing there.
	canSeeForeignProcesses() bool

	// inContainer reports container markers on the runner.
	inContainer() bool

	// titleByNamespacedPID finds a host process whose innermost namespaced PID is
	// pid. This covers PostgreSQL in a container with the agent on the host.
	// Linux only.
	titleByNamespacedPID(pid int) (string, bool)

	// parentStartTime is the start time of pid's parent. Used where there is no
	// readable process title.
	parentStartTime(pid int) (time.Time, bool)
}

// checkSameHost makes the decision. It does no I/O of its own, so every branch is
// testable with a fake processInspector. Order matters: a certain no takes
// priority over any local reading, and a missing process is only a no once the
// probe has confirmed it can see other users' processes.
func checkSameHost(f sameHostFacts, sys processInspector) sameHostResult {
	out := sameHostResult{evidence: collectEvidence(f)}

	// The agent cannot run on the machine hosting a managed database. Checked
	// first, so a runner with its own local PostgreSQL cannot produce a false yes.
	if f.managedService {
		out.verdict, out.reason = OnDBHostNo, hostReasonManagedService
		return out
	}

	if f.backendPID == "" {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonBackendPIDUnread
		return out
	}

	pid, err := strconv.Atoi(f.backendPID)
	if err != nil || pid <= 0 {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonBackendPIDUnread
		return out
	}

	// No title to read, so the only test left is the parent's start time.
	if !sys.titlesReadable() {
		if startTimeAgrees(f, sys, pid) {
			out.verdict, out.by = OnDBHostYes, confirmedByPostmasterStart
			return out
		}

		out.verdict, out.reason = OnDBHostUnknown, hostReasonPlatformNoTitles
		return out
	}

	title, visible := sys.title(pid)
	if !visible {
		return absentBackend(f, sys, pid, out)
	}

	if titleMatches(f, title) {
		out.verdict, out.by = OnDBHostYes, confirmedByBackendPID
		return out
	}

	// The process at that PID exists but its title is something else. Before
	// treating it as another machine's, try the namespace scan: when PostgreSQL
	// runs in a container, the host often has an unrelated process at the same
	// PID number, so a match on the number alone would find the wrong process.
	if title, ok := sys.titleByNamespacedPID(pid); ok && titleMatches(f, title) {
		out.verdict, out.by = OnDBHostYes, confirmedByBackendPIDNSpid
		return out
	}

	// An agent in a container has its own PID namespace, so a process it finds at
	// that number is always unrelated. Answering no here would be wrong for a
	// sidecar, which is one deployment change away from working, and a wrong no
	// is what switches host capture off.
	if sys.inContainer() {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonContainer
		return out
	}

	out.verdict, out.reason = OnDBHostNo, hostReasonTitleMismatch
	return out
}

// absentBackend tells apart the four causes of a PID the agent cannot see. Only
// one of them is a no, and it requires that the agent can see other users'
// processes.
func absentBackend(f sameHostFacts, sys processInspector, pid int, out sameHostResult) sameHostResult {
	// PostgreSQL in a container with the agent on the host: the most common Docker
	// setup, where vmstat and dmesg do describe the database's kernel.
	if title, ok := sys.titleByNamespacedPID(pid); ok && titleMatches(f, title) {
		out.verdict, out.by = OnDBHostYes, confirmedByBackendPIDNSpid
		return out
	}

	if !sys.canSeeForeignProcesses() {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonProcRestricted
		return out
	}

	if sys.inContainer() {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonContainer
		return out
	}

	// The PID is missing, other users' processes are visible, and neither side is
	// containerised, so the database is on another machine.
	out.verdict, out.reason = OnDBHostNo, hostReasonPIDAbsent
	return out
}

// titleMatches compares the title against values the server reported, never
// against configured ones. A pooler can map one user to another, and the config
// file can be wrong where the server's own answers cannot.
func titleMatches(f sameHostFacts, title string) bool {
	parsed, ok := parseBackendTitle(title)
	if !ok {
		return false
	}

	if parsed.role != f.role || parsed.database != f.database {
		return false
	}

	// Comparing the address and port is what rules out a PID collision. Matching
	// only the role and database is not enough: monitoring usernames are the same
	// across a fleet and the default database name is postgres, so a runner with
	// its own local cluster matches both.
	if !parsed.local {
		return f.clientAddr != "" && f.clientPort != "" &&
			sameAddr(parsed.addr, f.clientAddr) && parsed.port == f.clientPort
	}

	// A unix-socket connection has no address and port to compare, so both sides
	// must at least agree it is a socket. A forwarder that ends on the server's
	// own socket produces this same shape from a remote client, which was measured
	// and is why a socket title alone is not enough.
	return f.clientAddr == ""
}

// sameAddr compares two addresses as IPs where both parse, so two spellings of
// the same IPv6 address compare equal.
func sameAddr(a, b string) bool {
	if a == b {
		return true
	}

	ipA, ipB := net.ParseIP(a), net.ParseIP(b)

	return ipA != nil && ipB != nil && ipA.Equal(ipB)
}

// startTimeAgrees compares the backend's parent process start time against
// pg_postmaster_start_time(). The parent is the postmaster, so on the same
// machine the two readings come from one clock.
func startTimeAgrees(f sameHostFacts, sys processInspector, pid int) bool {
	if f.postmasterStart == "" {
		return false
	}

	want, err := time.Parse(timestampLayout, f.postmasterStart)
	if err != nil {
		return false
	}

	got, ok := sys.parentStartTime(pid)
	if !ok {
		return false
	}

	delta := got.Sub(want)
	if delta < 0 {
		delta = -delta
	}

	return delta <= postmasterStartTolerance
}

// collectEvidence lists the supporting signals seen. Each one can be faked, so
// none of them changes the verdict; they are recorded so a reader can see what
// information the run had.
func collectEvidence(f sameHostFacts) []string {
	var out []string

	if f.dialedSocket {
		out = append(out, evidenceClientSocket)
	}

	if f.logDirect {
		out = append(out, evidenceLogFile)
	}

	if f.serverAddr == "" {
		out = append(out, evidenceInetServerNull)
	} else if addrIsLocalInterface(f.serverAddr) {
		out = append(out, evidenceServerAddrMatch)
	}

	sort.Strings(out)

	return out
}

// addrIsLocalInterface reports whether the server's own view of its address is an
// address on one of this machine's interfaces. Recorded as evidence only: it is
// wrong where private address ranges overlap, because the agent's 10.0.0.5 need
// not be the server's 10.0.0.5.
func addrIsLocalInterface(addr string) bool {
	server := net.ParseIP(addr)
	if server == nil {
		return false
	}

	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}

	for _, a := range addrs {
		if ip, ok := a.(*net.IPNet); ok && ip.IP.Equal(server) {
			return true
		}
	}

	return false
}

// collectSameHost runs the probe and stores its answer. Call it after
// collectLogLocation, because log_access is one of the evidence inputs.
func collectSameHost(ctx context.Context, q Querier, m *Metadata, target Target) {
	facts := sameHostFacts{
		backendPID:      m.BackendPID,
		role:            m.CurrentUser,
		database:        m.CurrentDatabase,
		clientAddr:      m.InetClientAddr,
		clientPort:      m.InetClientPort,
		serverAddr:      m.InetServerAddr,
		postmasterStart: m.PostmasterStartTime,
		logDirect:       m.LogAccess == LogAccessDirect,
		dialedSocket:    strings.HasPrefix(target.Host, "/"),
		managedService:  managedService(ctx, q),
	}

	result := checkSameHost(facts, newProcessInspector(m.UpdateProcessTitle))

	m.AgentOnDBHost = result.verdict
	m.AgentOnDBHostBy = result.by
	m.AgentOnDBHostReason = result.reason
	m.AgentOnDBHostEvidence = strings.Join(result.evidence, ",")
}

// managedService returns false on any error. The check exists only to produce a
// certain no, so an error must never turn into one.
func managedService(ctx context.Context, q Querier) bool {
	ctx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var found *bool
	if err := q.QueryRow(ctx, managedServiceSQL).Scan(&found); err != nil {
		return false
	}

	return found != nil && *found
}
