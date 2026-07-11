//go:build !windows

package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"yc-agent/internal/logger"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

// nodeSignalTable maps accepted signal names to their syscall.Signal. SIGUSR1
// is deliberately absent — it is refused separately because it unconditionally
// activates Node's inspector (a security exposure).
var nodeSignalTable = map[string]syscall.Signal{
	"SIGUSR2": syscall.SIGUSR2,
	"SIGQUIT": syscall.SIGQUIT,
	"SIGABRT": syscall.SIGABRT,
}

func parseNodeSignal(name string) (syscall.Signal, error) {
	canonical, err := NormalizeNodeSignalName(name)
	if err != nil {
		return 0, err
	}
	sig, ok := nodeSignalTable[canonical]
	if !ok {
		return 0, fmt.Errorf("unsupported signal %q; use SIGUSR2 (matching the target's --report-on-signal)", name)
	}
	return sig, nil
}

type nodeReportSettings struct {
	dir      string
	filename string
}

func nodeReportSettingsForPID(pid int) nodeReportSettings {
	proc, err := psv3.NewProcess(int32(pid))
	if err != nil {
		return nodeReportSettings{dir: "."}
	}

	cwd := "."
	if procCwd, err := proc.Cwd(); err == nil && procCwd != "" {
		cwd = procCwd
	}

	var args []string
	if cmdlineArgs, err := proc.CmdlineSlice(); err == nil && len(cmdlineArgs) > 0 {
		args = cmdlineArgs
	} else if cmdline, err := proc.Cmdline(); err == nil && cmdline != "" {
		args = strings.Fields(cmdline)
	}

	settings := parseNodeReportSettingsFromArgs(args, cwd)
	if settings.dir == "" {
		settings.dir = cwd
	}
	return settings
}

func parseNodeReportSettingsFromArgs(args []string, cwd string) nodeReportSettings {
	settings := nodeReportSettings{dir: cwd}
	for i := 0; i < len(args); i++ {
		if value, ok := nodeFlagValue(args, &i,
			"--report-directory",
			"--report-dir",
			"--diagnostic-report-directory",
			"--diagnostic-report-dir",
		); ok {
			settings.dir = resolveNodeReportDir(value, cwd)
			continue
		}
		if value, ok := nodeFlagValue(args, &i,
			"--report-filename",
			"--diagnostic-report-filename",
		); ok {
			settings.filename = strings.TrimSpace(value)
		}
	}
	if settings.dir == "" {
		settings.dir = "."
	}
	return settings
}

func nodeFlagValue(args []string, idx *int, names ...string) (string, bool) {
	arg := args[*idx]
	for _, name := range names {
		if arg == name {
			if *idx+1 >= len(args) {
				return "", false
			}
			*idx++
			return strings.TrimSpace(args[*idx]), true
		}
		if value, ok := strings.CutPrefix(arg, name+"="); ok {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func resolveNodeReportDir(dir, cwd string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return cwd
	}
	if filepath.IsAbs(dir) || cwd == "" || cwd == "." {
		return filepath.Clean(dir)
	}
	return filepath.Clean(filepath.Join(cwd, dir))
}

func nodeReportFiles(settings nodeReportSettings) map[string]time.Time {
	out := make(map[string]time.Time)
	if entries, err := os.ReadDir(settings.dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, "report") || !strings.HasSuffix(name, ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			out[filepath.Join(settings.dir, name)] = info.ModTime()
		}
	}

	if customPath := nodeReportPath(settings); customPath != "" {
		if info, err := os.Stat(customPath); err == nil && !info.IsDir() {
			out[customPath] = info.ModTime()
		}
	}
	return out
}

func nodeReportPath(settings nodeReportSettings) string {
	filename := strings.TrimSpace(settings.filename)
	if filename == "" {
		return ""
	}
	if filepath.IsAbs(filename) {
		return filepath.Clean(filename)
	}
	return filepath.Clean(filepath.Join(settings.dir, filename))
}

// NodeSignalReportCapture sends the configured report signal to pid and polls
// the target's report directory for a newly-produced Diagnostic Report, then
// returns its path.
func NodeSignalReportCapture(pid int, signalName string, timeout time.Duration) (string, error) {
	sig, err := parseNodeSignal(signalName)
	if err != nil {
		return "", err
	}
	if !IsProcessExists(pid) {
		return "", fmt.Errorf("process %d does not exist", pid)
	}

	settings := nodeReportSettingsForPID(pid)
	before := nodeReportFiles(settings)
	sentAt := time.Now()

	if err := syscall.Kill(pid, sig); err != nil {
		return "", fmt.Errorf("failed sending %s to pid %d: %w", signalName, pid, err)
	}
	logger.Log("node signal mode: sent %s to pid %d, polling %s for a Diagnostic Report", signalName, pid, settings.dir)

	deadline := sentAt.Add(timeout)
	for time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)

		for path, mt := range nodeReportFiles(settings) {
			// New file, or an existing name rewritten after we sent the signal.
			if prev, existed := before[path]; !existed || mt.After(prev) || mt.After(sentAt) {
				if nodeFileStable(path) {
					return path, nil
				}
			}
		}

		if !IsProcessExists(pid) {
			return "", fmt.Errorf("process %d died before a report appeared", pid)
		}
	}

	return "", fmt.Errorf("timed out after %s waiting for a Diagnostic Report from pid %d in %s", timeout, pid, settings.dir)
}

func nodeFileStable(path string) bool {
	first, err := os.Stat(path)
	if err != nil || first.Size() == 0 {
		return false
	}
	time.Sleep(100 * time.Millisecond)
	second, err := os.Stat(path)
	if err != nil {
		return false
	}
	return second.Size() == first.Size()
}
