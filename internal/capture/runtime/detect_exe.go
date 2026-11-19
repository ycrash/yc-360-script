package runtime

import (
	"path/filepath"
	"strings"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

// runtimeByExecutableName maps well-known launcher names to their runtime.
var runtimeByExecutableName = map[string]RuntimeType{
	"java":       RuntimeJava,
	"java.exe":   RuntimeJava,
	"javaw":      RuntimeJava,
	"javaw.exe":  RuntimeJava,
	"node":       RuntimeNodeJS,
	"node.exe":   RuntimeNodeJS,
	"nodejs":     RuntimeNodeJS,
	"dotnet":     RuntimeDotNet,
	"dotnet.exe": RuntimeDotNet,
}

// RuntimeForExecutableName returns the runtime a launcher name such as
// "java.exe" or "node" belongs to, or RuntimeUnknown.
func RuntimeForExecutableName(name string) RuntimeType {
	if rt, ok := runtimeByExecutableName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return rt
	}
	return RuntimeUnknown
}

// ExecutableName returns the file name of the process's executable (for
// example "java.exe"), or "" when it can't be read.
func ExecutableName(pid int) string {
	if pid <= 0 {
		return ""
	}
	proc, err := psv3.NewProcess(int32(pid))
	if err != nil {
		return ""
	}
	if exe, err := proc.Exe(); err == nil && exe != "" {
		return cleanExecutableName(exe)
	}
	if name, err := proc.Name(); err == nil {
		return strings.TrimSpace(name)
	}
	return ""
}

// cleanExecutableName returns the file name of an executable path. On Linux,
// /proc/<pid>/exe reads "<path> (deleted)" when the binary was replaced or
// removed after start (e.g. a JDK upgrade while the app runs); drop that
// suffix so "java (deleted)" still counts as java.
func cleanExecutableName(exePath string) string {
	exePath = strings.TrimSuffix(strings.TrimSpace(exePath), " (deleted)")
	return filepath.Base(exePath)
}

// DetectProcessRuntime returns the runtime of pid only when it is certain:
// the executable is a well-known launcher (java, node, dotnet), or, on
// Windows, the loaded modules show a JVM or the CLR (not both). Anything else - custom
// launchers, embedded runtimes, unreadable processes - is RuntimeUnknown, so
// callers never reject a process on a guess. It also returns the executable
// name for messages.
func DetectProcessRuntime(pid int) (RuntimeType, string) {
	exe := ExecutableName(pid)
	if rt := RuntimeForExecutableName(exe); rt != RuntimeUnknown {
		return rt, exe
	}
	if info, err := DetectRuntime(pid); err == nil && info != nil && info.Runtime != RuntimeUnknown && !info.Ambiguous {
		return info.Runtime, exe
	}
	return RuntimeUnknown, exe
}
