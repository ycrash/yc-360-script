package capture

import (
	"fmt"
	"os"
	"strconv"

	"yc-agent/internal/logger"
)

const dotnetThreadOutputPath = "dotnet_thread.out"

// DotnetThread captures .NET thread dump.
type DotnetThread struct {
	Capture
	Pid int
}

// Run implements the capture by creating the output file, capturing thread dump,
// and then uploading the captured file.
func (d *DotnetThread) Run() (Result, error) {
	logger.Log("Starting .NET thread dump capture for PID %d", d.Pid)

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

// CaptureToFile captures the thread dump to a file and returns it.
func (d *DotnetThread) CaptureToFile() (*os.File, error) {
	// Build command arguments: --pid <pid> --thread
	args := []string{
		"--pid", strconv.Itoa(d.Pid),
		"--thread",
	}

	// Execute the dotnet tool and capture output
	file, err := executeDotnetTool(args, dotnetThreadOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET thread dump: %w", err)
	}

	logger.Log(".NET thread dump capture completed for PID %d", d.Pid)
	return file, nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "td".
func (d *DotnetThread) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "td", file)
	return Result{Msg: msg, Ok: ok}
}
