package capture

import (
	"fmt"
	"os"
	"strconv"
)

const dotnetGCOutputPath = "gc_output_%d.json"

// DotnetGC captures .NET garbage collection events.
type DotnetGC struct {
	Capture
	Pid      int
	Duration int // Duration in seconds for GC capture
}

// Run implements the capture by creating the output file, capturing GC events,
// and then uploading the captured file.
func (d *DotnetGC) Run() (Result, error) {
	capturedFile, err := d.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	return d.UploadCapturedFile(capturedFile), nil
}

// CaptureToFile captures the GC events to a file and returns it.
func (d *DotnetGC) CaptureToFile() (*os.File, error) {
	// Get current working directory
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	// Build command arguments: -gc <pid> <output_path>
	args := []string{
		"-gc",
		strconv.Itoa(d.Pid),
		workDir,
	}

	// Execute the dotnet tool and capture output
	file, err := executeDotnetTool(args, fmt.Sprintf(dotnetGCOutputPath, d.Pid))
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET GC events: %w", err)
	}

	return file, nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "gc".
func (d *DotnetGC) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "gc", file)
	return Result{Msg: msg, Ok: ok}
}
