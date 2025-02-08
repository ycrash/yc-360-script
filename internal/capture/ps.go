package capture

import (
	"fmt"
	"io"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const psOutputPath = "ps.out"

// PS handles the capture of process status data.
type PS struct {
	Capture
}

// NewPS creates a new PS capture instance.
func NewPS() *PS {
	return &PS{}
}

// Run executes the process status capture and uploads the captured file
// to the specified endpoint.
func (p *PS) Run() (Result, error) {
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
func (p *PS) CaptureToFile() (*os.File, error) {
	file, err := os.Create(psOutputPath)
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
func (p *PS) captureOutput(f *os.File) error {
	iterations := executils.SCRIPT_SPAN / executils.JAVACORE_INTERVAL

	if _, err := fmt.Fprintf(f, "\n%s\n", executils.NowString()); err != nil {
		return fmt.Errorf("failed to write timestamp: %w", err)
	}

	// Determine which PS command to use for all iterations
	// if PS failed, fallback to PS2
	psCmd := executils.PS
	if err := executils.CommandCombinedOutputToWriter(f, psCmd); err != nil {
		// Clear the file of any error output
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("failed to seek file: %w", err)
		}
		if err := f.Truncate(0); err != nil {
			return fmt.Errorf("failed to truncate file: %w", err)
		}

		logger.Log("trying %v, cause %v exit code != 0", executils.PS2, executils.PS)
		psCmd = executils.PS2

		if _, err := fmt.Fprintf(f, "\n%s\n", executils.NowString()); err != nil {
			return fmt.Errorf("failed to write timestamp: %w", err)
		}
		if err := executils.CommandCombinedOutputToWriter(f, psCmd); err != nil {
			return fmt.Errorf("both PS commands failed: %w", err)
		}
	}

	// Use the determined PS command for all subsequent iterations
	for i := 2; i < iterations; i++ {
		if _, err := fmt.Fprintf(f, "\n%s\n", executils.NowString()); err != nil {
			return fmt.Errorf("failed to write timestamp: %w", err)
		}

		if err := executils.CommandCombinedOutputToWriter(f, psCmd); err != nil {
			return fmt.Errorf("PS command failed during iteration %d: %w", i, err)
		}
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (p *PS) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(p.Endpoint(), "ps", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
