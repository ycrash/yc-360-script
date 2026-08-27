//go:build !linux && !windows

package postgres

import (
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
)

// unixInspector covers the BSDs, macOS and AIX: no /proc, so titles come from ps.
type unixInspector struct {
	updateProcessTitle string
}

func newProcessInspector(updateProcessTitle string) processInspector {
	return unixInspector{updateProcessTitle: updateProcessTitle}
}

// titlesReadable takes the server's setting at its word. The fixed part surviving
// it being off was confirmed on Linux only.
func (u unixInspector) titlesReadable() bool {
	return !strings.EqualFold(u.updateProcessTitle, "off")
}

// title uses ps rather than /proc, so this works on BSD.
func (unixInspector) title(pid int) (string, bool) {
	out, err := executils.CommandCombinedOutput(
		executils.Command{"ps", "-o", "command=", "-p", strconv.Itoa(pid)})
	if err != nil {
		return "", false
	}

	title := strings.TrimSpace(string(out))
	if title == "" {
		return "", false
	}

	return title, true
}

// canSeeForeignProcesses asks ps for PID 1: everywhere, and owned by no agent.
func (unixInspector) canSeeForeignProcesses() bool {
	out, err := executils.CommandCombinedOutput(
		executils.Command{"ps", "-o", "command=", "-p", "1"})

	return err == nil && strings.TrimSpace(string(out)) != ""
}

// inContainer is Linux-shaped, so no here - keeping an absent PID on the honest
// pid_absent rather than a vaguer unknown.
func (unixInspector) inContainer() bool { return false }

// titleByNamespacedPID has no equivalent: PID namespaces are Linux-only.
func (unixInspector) titleByNamespacedPID(int) (string, bool) { return "", false }

// parentStartTime uses ps twice. lstart is second-resolution, which the tolerance
// already allows for.
func (unixInspector) parentStartTime(pid int) (time.Time, bool) {
	out, err := executils.CommandCombinedOutput(
		executils.Command{"ps", "-o", "ppid=", "-p", strconv.Itoa(pid)})
	if err != nil {
		return time.Time{}, false
	}

	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid <= 0 {
		return time.Time{}, false
	}

	out, err = executils.CommandCombinedOutput(
		executils.Command{"ps", "-o", "lstart=", "-p", strconv.Itoa(ppid)})
	if err != nil {
		return time.Time{}, false
	}

	started, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006",
		strings.Join(strings.Fields(string(out)), " "), time.Local)
	if err != nil {
		return time.Time{}, false
	}

	return started.UTC(), true
}
