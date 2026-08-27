//go:build windows

package postgres

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
)

// windowsInspector has no title to read - the backend does not rewrite its command
// line here - so everything runs through the start-time test. Not hypothetical:
// this repository's 2026-08-19 run log is a Windows host.
type windowsInspector struct {
	updateProcessTitle string
}

func newProcessInspector(updateProcessTitle string) processInspector {
	return windowsInspector{updateProcessTitle: updateProcessTitle}
}

func (windowsInspector) titlesReadable() bool { return false }

func (windowsInspector) title(int) (string, bool) { return "", false }

// canSeeForeignProcesses is false: no title path means no absence to interpret,
// and claiming visibility would invite a no this platform cannot justify.
func (windowsInspector) canSeeForeignProcesses() bool { return false }

func (windowsInspector) inContainer() bool { return false }

func (windowsInspector) titleByNamespacedPID(int) (string, bool) { return "", false }

// parentStartTime goes through CIM like the tree's other Windows helpers, in two
// steps, avoiding the deprecated WMIC. Never exercised against a real Windows
// database host.
func (windowsInspector) parentStartTime(pid int) (time.Time, bool) {
	ppid, ok := windowsParentPID(pid)
	if !ok {
		return time.Time{}, false
	}

	out, err := executils.CommandCombinedOutput(executils.Command{
		executils.WaitCommand, "PowerShell.exe", "-Command",
		fmt.Sprintf("(Get-CimInstance Win32_Process -Filter \"ProcessId=%d\")"+
			".CreationDate.ToUniversalTime().ToString('o')", ppid),
	})
	if err != nil {
		return time.Time{}, false
	}

	started, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}, false
	}

	return started.UTC(), true
}

func windowsParentPID(pid int) (int, bool) {
	out, err := executils.CommandCombinedOutput(executils.Command{
		executils.WaitCommand, "PowerShell.exe", "-Command",
		fmt.Sprintf("(Get-CimInstance Win32_Process -Filter \"ProcessId=%d\").ParentProcessId", pid),
	})
	if err != nil {
		return 0, false
	}

	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || ppid <= 0 {
		return 0, false
	}

	return ppid, true
}
