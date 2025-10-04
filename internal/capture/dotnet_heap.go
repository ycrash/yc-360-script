package capture

import (
	"fmt"
	"os"
	"strconv"

	"yc-agent/internal/logger"
)

const dotnetHeapOutputPath = "dotnet_heap.out"

// DotnetHeap captures .NET heap statistics.
type DotnetHeap struct {
	Capture
	Pid int
}

// Run implements the capture by creating the output file, capturing heap statistics,
// and then uploading the captured file.
func (d *DotnetHeap) Run() (Result, error) {
	logger.Log("Starting .NET heap statistics capture for PID %d", d.Pid)

	// Check that the process exists
	if !IsProcessExists(d.Pid) {
		return Result{}, fmt.Errorf("process %d does not exist", d.Pid)
	}

	capturedFile, err := d.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	return d.UploadCapturedFile(capturedFile), nil
}

// CaptureToFile captures the heap statistics to a file and returns it.
func (d *DotnetHeap) CaptureToFile() (*os.File, error) {
	// Build command arguments: --pid <pid> --heap
	args := []string{
		"--pid", strconv.Itoa(d.Pid),
		"--heap",
	}

	// Execute the dotnet tool and capture output
	file, err := executeDotnetTool(args, dotnetHeapOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET heap statistics: %w", err)
	}

	logger.Log(".NET heap statistics capture completed for PID %d", d.Pid)
	return file, nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "hdsub".
func (d *DotnetHeap) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "hdsub", file)
	return Result{Msg: msg, Ok: ok}
}
