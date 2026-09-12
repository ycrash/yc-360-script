package capture

import (
	"encoding/json"
	"fmt"
	"os"

	"yc-agent/internal/logger"
)

const lpM3OutputPath = "lp.out"

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
	if len(p.Pids) == 0 {
		logger.Warn().Msg("LPM3.Run called with nil or empty Pids map, returning empty result")
		return Result{Msg: "no processes to capture", Ok: true}, nil
	}

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
	file, err := os.Create(lpM3OutputPath)
	if err != nil {
		return nil, fmt.Errorf("LPM3: failed to create output file %s: %w", lpM3OutputPath, err)
	}

	if err := p.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	// Ensures all file data is written to disk.
	if err := file.Sync(); err != nil {
		logger.Warn().Err(err).Msg("failed to sync file")
	}

	return file, nil
}

// captureOutput handles the actual process status capture process.
func (p *LPM3) captureOutput(f *os.File) error {
	logicalProcesses, err := p.collectLogicalProcesses()
	if err != nil {
		return err
	}

	encoder := json.NewEncoder(f)
	if err := encoder.Encode(logicalProcesses); err != nil {
		return fmt.Errorf("failed to encode logical processes to JSON: %w", err)
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (p *LPM3) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(p.Endpoint(), "lp", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
