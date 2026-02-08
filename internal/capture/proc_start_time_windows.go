//go:build windows

package capture

import (
	"fmt"
	"strconv"
	"strings"

	"yc-agent/internal/capture/executils"
)

func GetProcessStartTimestamp(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}

	cmd := executils.Command{
		executils.WaitCommand,
		"PowerShell.exe",
		"-Command",
		fmt.Sprintf("(Get-Process -Id %d -ErrorAction Stop).StartTime.ToFileTimeUtc()", pid),
	}

	output, err := executils.CommandCombinedOutput(cmd)
	if err != nil {
		return 0, err
	}

	value := strings.TrimSpace(string(output))
	ts, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed parsing process start time for pid=%d: %w", pid, err)
	}

	return ts, nil
}
