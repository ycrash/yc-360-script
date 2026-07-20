//go:build linux

package capture

import (
	"fmt"
	"os"
	"strconv"
)

// resolveNodeStdoutFile resolves fd 1 of the target process via
// /proc/<pid>/fd/1.
// It returns the path only if it resolves to a regular file.
func resolveNodeStdoutFile(pid int) (string, error) {
	fdPath := "/proc/" + strconv.Itoa(pid) + "/fd/1"
	target, err := os.Readlink(fdPath)
	if err != nil {
		return "", fmt.Errorf("failed resolving stdout (fd 1) for pid %d: %w", pid, err)
	}

	info, err := os.Stat(target)
	if err != nil {
		return "", fmt.Errorf("stdout of pid %d (%s) is not accessible: %w", pid, target, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("stdout of pid %d resolves to %s which is not a regular file (pipe/socket/device)", pid, target)
	}
	return target, nil
}
