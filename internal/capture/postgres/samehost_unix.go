//go:build !linux && !windows

package postgres

import (
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
)

// unixInspector covers the BSDs, macOS and AIX: no /proc to scan, so the title
// comes from ps. macOS suppresses backend titles unreliably, which is why an
// unmatched title there degrades to unknown rather than no - the caller reaches
// that through the ordinary title path.
type unixInspector struct {
	updateProcessTitle string
}

func newProcessInspector(updateProcessTitle string) processInspector {
	return unixInspector{updateProcessTitle: updateProcessTitle}
}

// titlesReadable is false when the server says it is not writing titles. On Linux
// the fixed part of the title survives the setting being off, but that was only
// confirmed there, so on these platforms the setting is taken at its word.
func (u unixInspector) titlesReadable() bool {
	return !strings.EqualFold(u.updateProcessTitle, "off")
}

// title reads the command with ps rather than a /proc path, so this works on BSD.
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

// canSeeForeignProcesses asks ps for PID 1, which exists everywhere and no
// unprivileged agent owns.
func (unixInspector) canSeeForeignProcesses() bool {
	out, err := executils.CommandCombinedOutput(
		executils.Command{"ps", "-o", "command=", "-p", "1"})

	return err == nil && strings.TrimSpace(string(out)) != ""
}

// inContainer is Linux-shaped detection; on these platforms the answer is no,
// which keeps an absent PID on the honest pid_absent rather than a vaguer
// unknown.
func (unixInspector) inContainer() bool { return false }

// titleByNamespacedPID has no equivalent outside Linux: PID namespaces are a
// Linux feature.
func (unixInspector) titleByNamespacedPID(int) (string, bool) { return "", false }

// parentStartTime resolves the parent with ps and reads its start time the same
// way. lstart is second-resolution, which the tolerance already allows for.
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
