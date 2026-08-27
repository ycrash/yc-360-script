package postgres

import (
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Is the agent on the machine running the database? Never a false yes: that would
// label another machine's CPU, memory and kernel readings as the database host's.
const (
	OnDBHostYes     = "yes"
	OnDBHostNo      = "no"
	OnDBHostUnknown = "unknown"
)

// Which test produced a yes, kept separate so a new test needs no new row.
const (
	confirmedByBackendPID      = "backend_pid"
	confirmedByBackendPIDNSpid = "backend_pid_nspid"
	confirmedByPostmasterStart = "postmaster_start_time"

	// confirmedByConfigured is a claim, not a reading.
	confirmedByConfigured = "configured"
)

// What the gate did, so a bundle with no host files explains itself.
const (
	HostArtifactsCaptured = "captured"
	HostArtifactsSkipped  = "skipped"
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
)

// Supporting signals. A tunnel or a pooler produces any of them from a machine
// that is not the host, so they are recorded and never decide the verdict.
const (
	evidenceClientSocket    = "client_socket"
	evidenceLogFile         = "log_file"
	evidenceServerAddrMatch = "server_addr_match"
	evidenceInetServerNull  = "inet_server_null"
)

// postmasterStartTolerance covers truncation on both sides; two readings of one
// start were measured 1s apart.
const postmasterStartTolerance = 2 * time.Second

// backendTitle is what a backend writes over its own argv:
//
//	postgres: <role> <database> <client_host>(<client_port>) <activity>
//	postgres: <role> <database> [local] <activity>
//
// The match stops before the activity, which is the only part
// update_process_title=off stops updating on Linux.
var backendTitle = regexp.MustCompile(
	`^postgres:\s+(?:\S+:\s+)?(\S+)\s+(\S+)\s+(?:\[local\]|([^\s(]+)\((\d+)\))`)

// parsedTitle is the fixed part of a backend title. local means a unix socket,
// which has no address and port to compare.
type parsedTitle struct {
	role     string
	database string
	addr     string
	port     string
	local    bool
}

// parseBackendTitle reports false for anything that is not a backend title, which
// is the common case for a colliding PID.
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

// sameHostFacts is what the server reported about this connection; empty means
// the query that would have set it did not run.
type sameHostFacts struct {
	backendPID      string
	role            string
	database        string
	clientAddr      string
	clientPort      string
	serverAddr      string
	postmasterStart string

	// logDirect is log_access == direct. Not proof: an NFS-mounted log directory
	// is readable from another machine.
	logDirect bool

	// dialedSocket is true when the agent dialed a unix socket path.
	dialedSocket bool
}

// sameHostResult is the probe's answer. reason is empty exactly when verdict is yes.
type sameHostResult struct {
	verdict  string
	by       string
	reason   string
	evidence []string
}

// processInspector reads the local process table. Linux implements every method;
// platforms with no readable title implement only parentStartTime.
type processInspector interface {
	titlesReadable() bool

	// title returns pid's command line. False means not visible, which has four
	// causes the caller must tell apart before answering no.
	title(pid int) (string, bool)

	// canSeeForeignProcesses is false under hidepid, where a missing process
	// means nothing.
	canSeeForeignProcesses() bool

	inContainer() bool

	// titleByNamespacedPID finds the host process whose innermost namespaced PID
	// is pid: PostgreSQL in a container, agent on the host. Linux only.
	titleByNamespacedPID(pid int) (string, bool)

	parentStartTime(pid int) (time.Time, bool)
}

// checkSameHost decides. No I/O of its own, so every branch is testable with a
// fake inspector. Order matters: a missing process is a no only once the probe
// has confirmed it can see other users' processes.
func checkSameHost(f sameHostFacts, sys processInspector) sameHostResult {
	out := sameHostResult{evidence: collectEvidence(f)}

	if f.backendPID == "" {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonBackendPIDUnread
		return out
	}

	pid, err := strconv.Atoi(f.backendPID)
	if err != nil || pid <= 0 {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonBackendPIDUnread
		return out
	}

	// No title to read, so the parent's start time is the only test left.
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

	// Something else holds that PID. With PostgreSQL in a container the host often
	// does, so match on the number alone would find the wrong process.
	if title, ok := sys.titleByNamespacedPID(pid); ok && titleMatches(f, title) {
		out.verdict, out.by = OnDBHostYes, confirmedByBackendPIDNSpid
		return out
	}

	// A containerised agent has its own PID namespace, so whatever it found is
	// unrelated. A no here would be wrong for a sidecar, and a wrong no is what
	// switches host capture off.
	if sys.inContainer() {
		out.verdict, out.reason = OnDBHostUnknown, hostReasonContainer
		return out
	}

	out.verdict, out.reason = OnDBHostNo, hostReasonTitleMismatch
	return out
}

// absentBackend tells apart the four causes of an invisible PID. Only one is a
// no, and it needs the agent to be able to see other users' processes.
func absentBackend(f sameHostFacts, sys processInspector, pid int, out sameHostResult) sameHostResult {
	// PostgreSQL in a container, agent on the host: the common Docker setup, where
	// vmstat and dmesg do describe the database's kernel.
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

	// Missing, other users' processes visible, neither side containerised.
	out.verdict, out.reason = OnDBHostNo, hostReasonPIDAbsent
	return out
}

// titleMatches compares against what the server reported, never what was
// configured: a pooler can map one user to another, and the config file can be
// wrong where the server's own answers cannot.
func titleMatches(f sameHostFacts, title string) bool {
	parsed, ok := parseBackendTitle(title)
	if !ok {
		return false
	}

	if parsed.role != f.role || parsed.database != f.database {
		return false
	}

	// The address and port rule out a PID collision. Role and database alone do
	// not: monitoring usernames repeat across a fleet and the default database is
	// postgres, so a runner with its own cluster matches both.
	if !parsed.local {
		return f.clientAddr != "" && f.clientPort != "" &&
			sameAddr(parsed.addr, f.clientAddr) && parsed.port == f.clientPort
	}

	// Nothing to compare on a socket, so both sides must at least agree it is one.
	// A forwarder ending on the server's socket produces this shape from a remote
	// client - measured, and why a socket title alone is not enough.
	return f.clientAddr == ""
}

// sameAddr compares as IPs where both parse, so two spellings of one IPv6 address
// are equal.
func sameAddr(a, b string) bool {
	if a == b {
		return true
	}

	ipA, ipB := net.ParseIP(a), net.ParseIP(b)

	return ipA != nil && ipB != nil && ipA.Equal(ipB)
}

// startTimeAgrees compares the backend's parent against pg_postmaster_start_time().
// The parent is the postmaster, so on one machine both readings share a clock.
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

// collectEvidence lists the supporting signals seen. Each is spoofable, so none
// changes the verdict; they record what the run had to go on.
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

// addrIsLocalInterface reports whether the server's view of its own address is on
// one of this machine's interfaces. Evidence only: private ranges overlap, so the
// agent's 10.0.0.5 need not be the server's.
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

// collectSameHost runs the probe and stores its answer. It sends no statement:
// every server-side input is already in m. Call it after collectLogLocation,
// which sets one of the evidence inputs.
func collectSameHost(m *Metadata, target Target) {
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
	}

	result := checkSameHost(facts, newProcessInspector(m.UpdateProcessTitle))

	m.AgentOnDBHost = result.verdict
	m.AgentOnDBHostBy = result.by
	m.AgentOnDBHostReason = result.reason
	m.AgentOnDBHostEvidence = strings.Join(result.evidence, ",")

	// Settled here so every reader of a collected Metadata sees the decision that
	// follows from its verdict. ResolveHostDecision can still raise it.
	m.HostArtifacts = hostArtifactsDecision(*m)
}

// applyOnDBHostDeclaration folds postgres.agentOnDbHost into the measured verdict
// and reports a disagreement. A measurement always wins, so the declaration
// decides only what the probe left unknown - a database that never answered
// cannot be asked which machine it runs on.
func applyOnDBHostDeclaration(m *Metadata, declared bool) (contradicted bool) {
	if !declared || m.AgentOnDBHost == OnDBHostYes {
		return false
	}

	if m.AgentOnDBHost == OnDBHostNo {
		return true
	}

	m.AgentOnDBHost = OnDBHostYes
	m.AgentOnDBHostBy = confirmedByConfigured
	m.AgentOnDBHostReason = ""

	return false
}

// hostArtifactsDecision is the gate: host files describe the machine that ran the
// agent, so they are captured only where that is the database's machine.
func hostArtifactsDecision(m Metadata) string {
	if m.AgentOnDBHost == OnDBHostYes {
		return HostArtifactsCaptured
	}

	return HostArtifactsSkipped
}

// HostCaptureHint maps a skip reason to the deployment change that would lift it,
// or to a plain statement where nothing can. Support reads the agent log first.
func HostCaptureHint(reason string) string {
	switch reason {
	case hostReasonContainer:
		return "the agent runs in a container and cannot see the database's processes; " +
			"if they share a kernel, run the agent with --pid=host"

	case hostReasonProcRestricted:
		return "/proc is mounted with hidepid, so the backend process is invisible to this user; " +
			"run the agent as the postgres OS user"

	case hostReasonPlatformNoTitles:
		return "this platform publishes no process titles, so confirmation needs the postmaster " +
			"start time, which this run could not read"

	case hostReasonBackendPIDUnread:
		return "pg_backend_pid() was not read, so there was no process to look for"

	case hostReasonNoConnection:
		return "the database never answered; set agentOnDbHost: true in the postgres: block to " +
			"declare that this machine runs it"

	case hostReasonPIDAbsent, hostReasonTitleMismatch:
		return "the database runs on another machine"
	}

	return ""
}
