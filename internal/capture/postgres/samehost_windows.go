//go:build windows

package postgres

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
)

// windowsInspector has no title to read: update_process_title defaults off on
// Windows and the backend does not rewrite its command line there. Everything
// therefore runs through the title-free test - the backend's parent started when
// pg_postmaster_start_time() says the postmaster did.
//
// This is not hypothetical: the run log in this repository from 2026-08-19 is a
// Windows host, and Windows is a documented platform in the README.
type windowsInspector struct {
	updateProcessTitle string
}

func newProcessInspector(updateProcessTitle string) processInspector {
	return windowsInspector{updateProcessTitle: updateProcessTitle}
}

func (windowsInspector) titlesReadable() bool { return false }

func (windowsInspector) title(int) (string, bool) { return "", false }

// canSeeForeignProcesses is false: with no title path there is no absence to
// interpret, and claiming visibility would only invite a no this platform cannot
// justify.
func (windowsInspector) canSeeForeignProcesses() bool { return false }

func (windowsInspector) inContainer() bool { return false }

func (windowsInspector) titleByNamespacedPID(int) (string, bool) { return "", false }

// parentStartTime goes through CIM, the same path the tree's other Windows
// process helpers use. Get-CimInstance carries ParentProcessId and CreationDate
// together, so the parent is resolved and read in two steps without WMIC, which
// is deprecated on current Windows.
//
// The arithmetic here is unmeasured - the one leg of the probe that is (§5.2).
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
