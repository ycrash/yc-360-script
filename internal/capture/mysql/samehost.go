package mysql

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/process"

	"yc-agent/internal/capture/dbcore"
)

// The same-host test (spec §1.5's hostMetricsAvailable). PostgreSQL forks a backend
// per connection whose title names it; MySQL is one mysqld process for every
// connection, so the test is the server's own start: a mysqld or mariadbd process
// on this machine that started when the connected server says it did. Never a
// false yes: that would file another machine's CPU, memory and kernel readings
// under the database.

// Which test produced a yes.
const (
	hostConfirmedByStartTime = "process_start_time"

	// hostConfirmedByConfigured is a claim, not a reading.
	hostConfirmedByConfigured = "configured"
)

// Why the verdict is not yes. Mandatory whenever it is not.
const (
	hostReasonNoConnection     = "no_connection"
	hostReasonUptimeUnread     = "uptime_unread"
	hostReasonProcessesUnread  = "process_table_unreadable"
	hostReasonContainer        = "container"
	hostReasonNoServerProcess  = "no_server_process"
	hostReasonStartMismatch    = "start_time_mismatch"
	hostReasonForeignProcesses = "foreign_processes_hidden"
)

// Supporting signals, recorded and never deciding the verdict: a shared mount or a
// tunnel produces either from another machine.
const (
	evidencePidFile = "pid_file"
	evidenceSocket  = "socket"
	evidenceLogFile = "log_file"
)

// startTolerance covers Uptime's whole seconds on one side and the clock read on
// the other.
const startTolerance = 2 * time.Second

// serverProcessNames are the server's executable names.
var serverProcessNames = []string{"mysqld", "mariadbd"}

// serverProcess is one local process that could be the server.
type serverProcess struct {
	pid     int32
	started time.Time
}

// processLister reads the local process table: the server processes it can see,
// and whether it can see other users' processes at all.
type processLister interface {
	serverProcesses() (processes []serverProcess, foreignVisible bool, ok bool)
	inContainer() bool
}

// localProcesses is the real process table.
type localProcesses struct{}

func (localProcesses) serverProcesses() ([]serverProcess, bool, bool) {
	all, err := process.Processes()
	if err != nil {
		return nil, false, false
	}

	var found []serverProcess

	foreignVisible := false
	self := int32(os.Getpid())

	for _, p := range all {
		// PID 1 belongs to another user on every system the agent runs on; a
		// hidepid mount hides it, and then a missing server proves nothing.
		if p.Pid == 1 && p.Pid != self {
			foreignVisible = true
		}

		name, err := p.Name()
		if err != nil || !isServerProcessName(name) {
			continue
		}

		created, err := p.CreateTime()
		if err != nil {
			continue
		}

		found = append(found, serverProcess{pid: p.Pid, started: time.UnixMilli(created)})
	}

	return found, foreignVisible, true
}

// inContainer: the files the two common runtimes leave at the root.
func (localProcesses) inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}

	return false
}

func isServerProcessName(name string) bool {
	for _, server := range serverProcessNames {
		if name == server || strings.HasPrefix(name, server+"-") {
			return true
		}
	}

	return false
}

// collectSameHost decides agent_on_db_host from the server's uptime, read at now,
// and the local process table.
func collectSameHost(m *Metadata, inspect processLister, now time.Time) {
	if inspect == nil {
		inspect = localProcesses{}
	}

	m.AgentOnDBHost = dbcore.OnDBHostUnknown
	m.AgentOnDBHostBy = ""

	uptime, err := strconv.ParseInt(m.UptimeSeconds, 10, 64)
	if err != nil {
		m.AgentOnDBHostReason = hostReasonUptimeUnread

		return
	}

	started := now.Add(-time.Duration(uptime) * time.Second)

	processes, foreignVisible, ok := inspect.serverProcesses()

	switch {
	case !ok:
		m.AgentOnDBHostReason = hostReasonProcessesUnread

		return

	case len(processes) == 0 && inspect.inContainer():
		// The agent's container hides the host's processes, and the server may be
		// in another container on the same machine.
		m.AgentOnDBHostReason = hostReasonContainer

		return

	case len(processes) == 0 && !foreignVisible:
		m.AgentOnDBHostReason = hostReasonForeignProcesses

		return

	case len(processes) == 0:
		m.AgentOnDBHost = dbcore.OnDBHostNo
		m.AgentOnDBHostReason = hostReasonNoServerProcess

		return
	}

	for _, p := range processes {
		gap := p.started.Sub(started)
		if gap < 0 {
			gap = -gap
		}

		if gap <= startTolerance {
			m.AgentOnDBHost = dbcore.OnDBHostYes
			m.AgentOnDBHostBy = hostConfirmedByStartTime
			m.AgentOnDBHostReason = ""
			m.AgentOnDBHostEvidence = strings.Join(sameHostEvidence(m, p.pid), ",")

			return
		}
	}

	// A server runs here, but not the one connected to: its start disagrees.
	m.AgentOnDBHost = dbcore.OnDBHostNo
	m.AgentOnDBHostReason = hostReasonStartMismatch
}

// sameHostEvidence lists the supporting signals: the pid file names the process, the
// socket exists, the error log opened.
func sameHostEvidence(m *Metadata, pid int32) []string {
	var evidence []string

	if content, err := os.ReadFile(m.PidFile); err == nil && strings.TrimSpace(string(content)) == strconv.Itoa(int(pid)) {
		evidence = append(evidence, evidencePidFile)
	}

	if info, err := os.Stat(m.Socket); err == nil && info.Mode()&os.ModeSocket != 0 {
		evidence = append(evidence, evidenceSocket)
	}

	if m.LogAccess == LogAccessDirect {
		evidence = append(evidence, evidenceLogFile)
	}

	return evidence
}
