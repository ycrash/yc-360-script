//go:build !windows

package capture

import (
	"yc-agent/internal/logger"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

// collectLogicalProcesses gathers the logical processes for the captured pids.
func (p *LPM3) collectLogicalProcesses() ([]LogicalProcess, error) {
	var logicalProcesses []LogicalProcess

	for pid := range p.Pids {
		process, err := psv3.NewProcess(int32(pid))
		if err != nil {
			logger.Warn().Err(err).Int("pid", pid).Msg("LPM3: failed to create process object")
			continue
		}

		psName, err := process.Name()
		if err != nil {
			logger.Warn().Err(err).Int("pid", pid).Msg("LPM3: failed to get process name")
			psName = ""
		}

		cmdLine, err := process.Cmdline()
		if err != nil {
			logger.Warn().Err(err).Int("pid", pid).Msg("LPM3: failed to get command line")
			cmdLine = ""
		}

		logicalProcesses = append(logicalProcesses, LogicalProcess{
			ProcessName: psName,
			ProcessId:   pid,
			CommandLine: cmdLine,
		})
	}

	return logicalProcesses, nil
}
