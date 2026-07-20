//go:build darwin

package capture

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"yc-agent/internal/capture/executils"
)

// resolveNodeStdoutFile resolves fd 1 of the target process on macOS using
// `lsof -p <pid> -a -d 1 -Fn` (fd 1 only, name field). It returns the path only
// if it resolves to a regular file.
func resolveNodeStdoutFile(pid int) (string, error) {
	pidStr := strconv.Itoa(pid)
	output, err := executils.CommandCombinedOutput(executils.Command{"lsof", "-p", pidStr, "-a", "-d", "1", "-Fn"})
	if err != nil {
		return "", fmt.Errorf("failed resolving stdout (fd 1) for pid %d: %w", pid, err)
	}

	var target string
	s := bufio.NewScanner(bytes.NewReader(output))
	for s.Scan() {
		line := s.Text()
		if path, ok := strings.CutPrefix(line, "n"); ok && path != "" {
			target = path
			break
		}
	}
	if target == "" {
		return "", fmt.Errorf("could not determine stdout (fd 1) path for pid %d", pid)
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
