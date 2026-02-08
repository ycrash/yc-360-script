//go:build !windows

package capture

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
)

func GetProcessStartTimestamp(pid int) (int64, error) {
	if pid <= 0 {
		return 0, fmt.Errorf("invalid pid %d", pid)
	}

	cmd := executils.Command{"ps", "-o", "lstart=", "-p", strconv.Itoa(pid)}
	output, err := executils.CommandCombinedOutput(cmd)
	if err != nil {
		return 0, err
	}

	value := strings.TrimSpace(string(output))
	if value == "" {
		return 0, fmt.Errorf("process not found: %d", pid)
	}

	tm, err := time.Parse("Mon Jan 2 15:04:05 2006", value)
	if err != nil {
		return 0, fmt.Errorf("failed parsing process start time for pid=%d: %w", pid, err)
	}

	return tm.UnixNano(), nil
}
