package capture

import (
	"fmt"
	"os"
	"strconv"

	"yc-agent/internal/logger"
)

const dotnetGCOutputPath = "dotnet_gc.out"

// DotnetGC captures .NET garbage collection events.
type DotnetGC struct {
	Capture
	Pid      int
	Duration int // Duration in seconds for GC capture
}

// Run implements the capture by creating the output file, capturing GC events,
// and then uploading the captured file.
func (d *DotnetGC) Run() (Result, error) {
	logger.Log("Starting .NET GC capture for PID %d (duration: %d seconds)", d.Pid, d.Duration)

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

// CaptureToFile captures the GC events to a file and returns it.
func (d *DotnetGC) CaptureToFile() (*os.File, error) {
	// Build command arguments: --pid <pid> --duration <duration> --gc
	args := []string{
		"--pid", strconv.Itoa(d.Pid),
		"--duration", strconv.Itoa(d.Duration),
		"--gc",
	}

	// Execute the dotnet tool and capture output
	file, err := executeDotnetTool(args, dotnetGCOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET GC events: %w", err)
	}

	logger.Log(".NET GC capture completed for PID %d", d.Pid)
	return file, nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "gc".
func (d *DotnetGC) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "gc", file)
	return Result{Msg: msg, Ok: ok}
}
