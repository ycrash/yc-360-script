package capture

import (
	"fmt"
	"os"
	"yc-agent/internal/capture/executils"
)

const outputFile = "disk.out"

// Disk represents a disk metrics collector.
// It gathers disk usage statistics and uploads them to a specified endpoint.
type Disk struct {
	Capture
}

// Run collects and uploads the disk metrics collection.
func (d *Disk) Run() (Result, error) {
	file, err := d.CaptureToFile()
	if err != nil {
		return Result{}, fmt.Errorf("failed to capture disk metrics: %w", err)
	}
	defer file.Close()

	return d.UploadCapturedFile(file)
}

// CaptureToFile executes the disk metrics collection command and saves output to a file.
func (d *Disk) CaptureToFile() (*os.File, error) {
	file, err := executils.CommandCombinedOutputToFile(outputFile, executils.Disk)
	if err != nil {
		return nil, fmt.Errorf("failed to execute disk command: %w", err)
	}

	return file, nil
}

// UploadCapturedFile sends the collected disk metrics to the configured endpoint.
func (d *Disk) UploadCapturedFile(file *os.File) (Result, error) {
	msg, ok := PostData(d.endpoint, "df", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}, nil
}
