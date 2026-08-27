//go:build linux

package postgres

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procRoot is a variable so tests can point the inspector at a fixture tree.
var procRoot = "/proc"

// linuxInspector reads /proc directly: the namespace scan needs it anyway, and
// cmdline's NUL separation is what distinguishes an empty kernel thread from a
// backend title.
type linuxInspector struct {
	// Recorded, not acted on: on Linux the title's fixed part survives it being off.
	updateProcessTitle string
}

func newProcessInspector(updateProcessTitle string) processInspector {
	return linuxInspector{updateProcessTitle: updateProcessTitle}
}

func (linuxInspector) titlesReadable() bool { return true }

func (linuxInspector) title(pid int) (string, bool) {
	return readCmdline(filepath.Join(procRoot, strconv.Itoa(pid)))
}

// readCmdline joins cmdline's NUL-separated argv; a backend that rewrote its argv
// appears as one long argument.
func readCmdline(dir string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, "cmdline"))
	if err != nil {
		return "", false
	}

	trimmed := bytes.TrimRight(raw, "\x00")
	if len(trimmed) == 0 {
		// A kernel thread: visible, nothing to match. The usual shape of a PID
		// collision in the host namespace.
		return "", true
	}

	return string(bytes.ReplaceAll(trimmed, []byte{0}, []byte{' '})), true
}

// canSeeForeignProcesses tests PID 1, which every /proc has and no unprivileged
// agent owns. Unreadable under hidepid, where an absent backend proves nothing.
func (linuxInspector) canSeeForeignProcesses() bool {
	_, err := os.ReadFile(filepath.Join(procRoot, "1", "cmdline"))

	return err == nil
}

func (linuxInspector) inContainer() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}

	cgroup, err := os.ReadFile(filepath.Join(procRoot, "1", "cgroup"))
	if err != nil {
		return false
	}

	text := string(cgroup)

	return strings.Contains(text, "docker") ||
		strings.Contains(text, "kubepods") ||
		strings.Contains(text, "containerd") ||
		strings.Contains(text, "libpod")
}

// titleByNamespacedPID scans for the host process whose innermost NSpid equals
// pid - PostgreSQL in a container, agent on the host. NSpid lists a PID in every
// namespace it belongs to, innermost last; no root needed. It returns the title,
// not a verdict, because the root namespace routinely holds an unrelated process
// at the same bare number.
func (linuxInspector) titleByNamespacedPID(pid int) (string, bool) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return "", false
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		hostPID, err := strconv.Atoi(entry.Name())
		if err != nil || hostPID == pid {
			// Skip the bare-number match itself: the caller has already tried it.
			continue
		}

		if innermostNSpid(entry.Name()) != pid {
			continue
		}

		if title, ok := readCmdline(filepath.Join(procRoot, entry.Name())); ok && title != "" {
			return title, true
		}
	}

	return "", false
}

// innermostNSpid returns the last field of /proc/<pid>/status's NSpid line, or 0
// when the line is absent (a kernel without PID namespaces).
func innermostNSpid(name string) int {
	status, err := os.ReadFile(filepath.Join(procRoot, name, "status"))
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}

		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			return 0
		}

		innermost, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			return 0
		}

		return innermost
	}

	return 0
}

// parentStartTime reads the postmaster's start as btime + starttime/CLK_TCK.
// Measured 1s off pg_postmaster_start_time(), from truncation on both sides.
func (linuxInspector) parentStartTime(pid int) (time.Time, bool) {
	ppid, ok := parentPID(pid)
	if !ok {
		return time.Time{}, false
	}

	ticks, ok := startTimeTicks(ppid)
	if !ok {
		return time.Time{}, false
	}

	boot, ok := bootTime()
	if !ok {
		return time.Time{}, false
	}

	// USER_HZ, not the kernel's CONFIG_HZ. 100 on every port the agent supports.
	const userHZ = 100

	return boot.Add(time.Duration(ticks) * time.Second / userHZ), true
}

func parentPID(pid int) (int, bool) {
	status, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "status"))
	if err != nil {
		return 0, false
	}

	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}

		ppid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		if err != nil {
			return 0, false
		}

		return ppid, true
	}

	return 0, false
}

// startTimeTicks reads field 22 of stat, parsing after the final ')' because
// field 2 is the executable name and may contain spaces and parentheses.
func startTimeTicks(pid int) (int64, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}

	close := bytes.LastIndexByte(raw, ')')
	if close < 0 {
		return 0, false
	}

	// After the name, field 3 is state; starttime is field 22, so index 19 here.
	fields := strings.Fields(string(raw[close+1:]))
	const startTimeIndex = 19

	if len(fields) <= startTimeIndex {
		return 0, false
	}

	ticks, err := strconv.ParseInt(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, false
	}

	return ticks, true
}

func bootTime() (time.Time, bool) {
	stat, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, false
	}

	for _, line := range strings.Split(string(stat), "\n") {
		if !strings.HasPrefix(line, "btime ") {
			continue
		}

		seconds, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
		if err != nil {
			return time.Time{}, false
		}

		return time.Unix(seconds, 0).UTC(), true
	}

	return time.Time{}, false
}
