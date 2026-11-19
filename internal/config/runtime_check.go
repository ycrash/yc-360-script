package config

import (
	"fmt"

	"yc-agent/internal/capture/runtime"
)

// ProcessRuntimeDetector inspects a process; a variable so tests (here and in
// the agent packages) can stub it.
var ProcessRuntimeDetector = runtime.DetectProcessRuntime

var runtimeDisplayNames = map[string]string{
	"java":   "Java",
	"nodejs": "Node.js",
	"dotnet": ".NET",
}

// CheckAppRuntimeMatches returns an error when -appRuntime is set and pid is
// certainly a different runtime, e.g. -appRuntime nodejs with a java.exe PID.
// Running the wrong runtime's captures would only produce "skipped" results,
// so callers stop (on-demand) or skip the PID (M3, API). It returns nil when
// -appRuntime is not set or the process's runtime can't be determined for sure.
func CheckAppRuntimeMatches(pid int) error {
	configured := GetConfiguredAppRuntime()
	if configured == "" {
		return nil
	}

	actual, exe := ProcessRuntimeDetector(pid)
	if actual == runtime.RuntimeUnknown || string(actual) == configured {
		return nil
	}

	process := runtimeDisplayNames[string(actual)] + " process"
	if exe != "" {
		process += fmt.Sprintf(" (%s)", exe)
	}
	// Logged as-is as a user-facing ERR line, so it is written as full sentences.
	return fmt.Errorf("PID %d is a %s, but -appRuntime %s was passed. Pass -appRuntime %s, or remove -appRuntime and the yc-360 script will detect the runtime automatically.", //nolint:staticcheck // ST1005: a user-facing message, logged as full sentences
		pid, process, configured, actual)
}
