package mysql

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v3/process"

	"yc-agent/internal/capture/dbcore"
)

// Same-host test (spec §1.5 hostMetricsAvailable): a local mysqld/mariadbd whose
// start matches the server's uptime. Never a false yes.

// Which test produced a yes.
const (
	hostConfirmedByStartTime = "process_start_time"

	// hostConfirmedByThreadStart: the process predates the server but its threads
	// don't. The Docker entrypoint execs mysqld after init, keeping the shell's
	// start (6.6s earlier on the matrix).
	hostConfirmedByThreadStart = "thread_start_time"

	// hostConfirmedByConfigured is a claim, not a reading.
	hostConfirmedByConfigured = "configured"
)

// Why the verdict is not yes; always set when it isn't.
const (
	hostReasonNoConnection     = "no_connection"
	hostReasonUptimeUnread     = "uptime_unread"
	hostReasonProcessesUnread  = "process_table_unreadable"
	hostReasonContainer        = "container"
	hostReasonNoServerProcess  = "no_server_process"
	hostReasonStartMismatch    = "start_time_mismatch"
	hostReasonForeignProcesses = "foreign_processes_hidden"
)

// Supporting signals, recorded but never deciding: a shared mount or tunnel fakes them.
const (
	evidencePidFile = "pid_file"
	evidenceSocket  = "socket"
	evidenceLogFile = "log_file"
)

// startTolerance covers Uptime's whole-second resolution.
const startTolerance = 2 * time.Second

var serverProcessNames = []string{"mysqld", "mariadbd"}

// serverProcess is one local process that could be the server.
type serverProcess struct {
	pid     int32
	started time.Time

	// threadsStarted is its earliest non-main thread's start; zero if unknown.
	threadsStarted time.Time
}

// processLister reads the local process table.
type processLister interface {
	serverProcesses() (processes []serverProcess, foreignVisible bool, ok bool)
	inContainer() bool
}

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
		// Seeing PID 1 means hidepid isn't hiding other users' processes.
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

		started := time.UnixMilli(created)

		found = append(found, serverProcess{
			pid:            p.Pid,
			started:        started,
			threadsStarted: firstThreadStart("/proc", p.Pid, started),
		})
	}

	return found, foreignVisible, true
}

// inContainer checks the Docker and Podman marker files.
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

// collectSameHost decides agent_on_db_host from the server's uptime and local processes.
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
		// The server may be in a sibling container we can't see.
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
		var by string

		switch {
		case startsTogether(p.started, started):
			by = hostConfirmedByStartTime

		case p.started.Before(started) && !p.threadsStarted.IsZero() && startsTogether(p.threadsStarted, started):
			by = hostConfirmedByThreadStart

		default:
			continue
		}

		m.AgentOnDBHost = dbcore.OnDBHostYes
		m.AgentOnDBHostBy = by
		m.AgentOnDBHostReason = ""
		m.AgentOnDBHostEvidence = strings.Join(sameHostEvidence(m, p.pid), ",")

		return
	}

	// A local server, but not the connected one.
	m.AgentOnDBHost = dbcore.OnDBHostNo
	m.AgentOnDBHostReason = hostReasonStartMismatch
}

func startsTogether(a, b time.Time) bool {
	gap := a.Sub(b)

	return gap >= -startTolerance && gap <= startTolerance
}

// userHZ is USER_HZ, the tick unit of /proc/<pid>/stat start times.
const userHZ = 100

// firstThreadStart is when pid's earliest non-main thread began, relative to
// started. Zero without /proc or other threads.
func firstThreadStart(proc string, pid int32, started time.Time) time.Time {
	id := strconv.Itoa(int(pid))

	own, ok := startTicks(filepath.Join(proc, id, "stat"))
	if !ok {
		return time.Time{}
	}

	tasks, err := os.ReadDir(filepath.Join(proc, id, "task"))
	if err != nil {
		return time.Time{}
	}

	first := uint64(math.MaxUint64)

	for _, task := range tasks {
		if task.Name() == id {
			continue
		}

		if ticks, ok := startTicks(filepath.Join(proc, id, "task", task.Name(), "stat")); ok {
			first = min(first, ticks)
		}
	}

	if first == math.MaxUint64 || first < own {
		return time.Time{}
	}

	return started.Add(time.Duration(first-own) * time.Second / userHZ)
}

// startTicks is a stat file's starttime (field 22), parsed past the command's
// last ')' since the command may contain spaces and parentheses.
func startTicks(statFile string) (uint64, bool) {
	content, err := os.ReadFile(statFile)
	if err != nil {
		return 0, false
	}

	end := strings.LastIndexByte(string(content), ')')
	if end < 0 {
		return 0, false
	}

	// Fields from the third (state) on.
	fields := strings.Fields(string(content[end+1:]))
	if len(fields) < 20 {
		return 0, false
	}

	ticks, err := strconv.ParseUint(fields[19], 10, 64)

	return ticks, err == nil
}

// sameHostEvidence lists the supporting signals present.
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
