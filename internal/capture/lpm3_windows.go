//go:build windows

package capture

import (
	"fmt"

	"yc-agent/internal/config"
)

// collectLogicalProcesses gathers the logical processes via CIM, which covers
// every matching process on the host rather than only the captured pids.
func (p *LPM3) collectLogicalProcesses() ([]LogicalProcess, error) {
	processes, err := GetCIMProcesses(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	if err != nil {
		return nil, fmt.Errorf("LPM3: failed to get CIM processes: %w", err)
	}

	var logicalProcesses []LogicalProcess
	for _, process := range processes {
		logicalProcesses = append(logicalProcesses, LogicalProcess(process))
	}

	return logicalProcesses, nil
}
