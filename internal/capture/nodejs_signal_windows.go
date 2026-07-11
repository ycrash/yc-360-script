//go:build windows

package capture

import (
	"fmt"
	"time"
)

// NodeSignalReportCapture is unsupported on Windows: there is no usable external
// signal-delivery mechanism (SIGUSR1/2 don't exist; SIGINT/SIGBREAK/SIGHUP all
// have disqualifying semantics).
func NodeSignalReportCapture(pid int, signalName string, timeout time.Duration) (string, error) {
	return "", fmt.Errorf("node signal mode is not supported on Windows (pid %d)", pid)
}
