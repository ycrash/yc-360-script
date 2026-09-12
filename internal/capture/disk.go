package capture

import (
	"fmt"
	"os"
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

// UploadCapturedFile sends the collected disk metrics to the configured endpoint.
func (d *Disk) UploadCapturedFile(file *os.File) (Result, error) {
	msg, ok := PostData(d.endpoint, "df", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}, nil
}
