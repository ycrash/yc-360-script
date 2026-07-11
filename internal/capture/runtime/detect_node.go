package runtime

import (
	"path/filepath"
	"strings"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

var nodeExecutableNames = map[string]struct{}{
	"node":     {},
	"node.exe": {},
	"nodejs":   {},
}

// IsNodeProcess reports whether the process identified by pid appears to be a
// Node.js process.
func IsNodeProcess(pid int) bool {
	if pid <= 0 {
		return false
	}

	proc, err := psv3.NewProcess(int32(pid))
	if err != nil {
		return false
	}

	if exe, err := proc.Exe(); err == nil && exe != "" {
		if isNodeExecutableName(filepath.Base(exe)) {
			return true
		}
	}

	if name, err := proc.Name(); err == nil && name != "" {
		if isNodeExecutableName(name) {
			return true
		}
	}

	return false
}

func isNodeExecutableName(name string) bool {
	_, ok := nodeExecutableNames[strings.ToLower(strings.TrimSpace(name))]
	return ok
}
