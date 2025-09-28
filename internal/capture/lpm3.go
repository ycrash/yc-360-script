package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

const lsM3OutputPath = "lp.out"

// LPM3 (Logical Process) handles the capture of process status data.
type LPM3 struct {
	Capture
	Pids map[int]string
}

type LogicalProcess struct {
	ProcessName string
	CommandLine string
	ProcessId   int
}

// NewLPM3 creates a new LPM3 capture instance.
func NewLPM3(pids map[int]string) *LPM3 {
	return &LPM3{Pids: pids}
}

// Run executes the process status capture and uploads the captured file
// to the specified endpoint.
func (p *LPM3) Run() (Result, error) {
	capturedFile, err := p.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := p.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile captures process status output to a file.
// It returns the file handle for the captured data.
func (p *LPM3) CaptureToFile() (*os.File, error) {
	file, err := os.Create(lsM3OutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := p.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	// Ensures all file data is written to disk.
	if err := file.Sync(); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return file, nil
}

// captureOutput handles the actual process status capture process.
func (p *LPM3) captureOutput(f *os.File) error {
	if runtime.GOOS == "windows" {
		processes, err := GetCIMProcesses(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
		if err != nil {
			return err
		}

		logicalProcesses := []LogicalProcess{}

		for _, process := range processes {
			logicalProcesses = append(logicalProcesses, LogicalProcess{ProcessName: process.ProcessName, ProcessId: process.ProcessId, CommandLine: process.CommandLine})
		}

		bytes, err := json.Marshal(logicalProcesses)
		if err != nil {
			return err
		}

		_, err = f.Write(bytes)
		return err
	} else {
		logicalProcesses := []LogicalProcess{}

		for pid := range p.Pids {
			process, err := psv3.NewProcess(int32(pid))

			if err != nil {
				continue
			}

			psName, _ := process.Name()
			cmdLine, _ := process.Cmdline()
			logicalProcesses = append(logicalProcesses, LogicalProcess{ProcessName: psName, ProcessId: pid, CommandLine: cmdLine})
		}

		bytes, err := json.Marshal(logicalProcesses)
		if err != nil {
			return err
		}

		_, err = f.Write(bytes)
		return err
	}
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (p *LPM3) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(p.Endpoint(), "lp", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
