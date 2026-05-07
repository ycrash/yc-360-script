//go:build windows

package capture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows"
	"yc-agent/internal/config"
)

const (
	dotnetToolX86 = "yc-dot-net-x86.exe"
	dotnetToolX64 = "yc-dot-net-x64.exe"
)

// resolveDotnetToolByPid picks helper binary by target process architecture.
// Returns (toolPath, true, nil) when selection succeeded.
// Returns ("", false, nil) when no architecture-specific binary was found.
func resolveDotnetToolByPid(pid int) (string, bool, error) {
	targetArch, err := detectWindowsProcessArch(pid)
	if err != nil {
		return "", false, nil // non-fatal: caller will use default resolver
	}

	preferred := dotnetToolX64
	if targetArch == "x86" {
		preferred = dotnetToolX86
	}

	// Search order:
	// 1) sibling to yc executable
	// 2) PATH
	// 3) fallback default name (yc-dot-net.exe)
	if p, ok := findToolNearYcOrPath(preferred); ok {
		return p, true, nil
	}
	if p, ok := findToolNearYcOrPath(config.DefaultDotnetToolName); ok {
		return p, true, nil
	}

	return "", false, fmt.Errorf(".NET helper for target PID %d (%s) not found. expected %s (or %s)", pid, targetArch, preferred, config.DefaultDotnetToolName)
}

func findToolNearYcOrPath(toolName string) (string, bool) {
	if exePath, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exePath), toolName)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, true
		}
	}
	if resolved, err := exec.LookPath(toolName); err == nil {
		return resolved, true
	}
	return "", false
}

func detectWindowsProcessArch(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)

	var targetWow64 bool
	if err = windows.IsWow64Process(h, &targetWow64); err != nil {
		return "", err
	}

	// On 32-bit Windows, everything is x86.
	if isOS64Bit() {
		if targetWow64 {
			return "x86", nil
		}
		return "x64", nil
	}
	return "x86", nil
}

func isOS64Bit() bool {
	return os.Getenv("PROCESSOR_ARCHITEW6432") != "" || os.Getenv("PROCESSOR_ARCHITECTURE") == "AMD64"
}
