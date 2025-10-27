package capture

import (
	"fmt"
	"os"
	"strconv"
)

const dotnetThreadOutputPath = "thread_dump_%d.json"

// DotnetThread captures .NET thread dump.
type DotnetThread struct {
	Capture
	Pid int
}

// Run implements the capture by creating the output file, capturing thread dump,
// and then uploading the captured file.
func (d *DotnetThread) Run() (Result, error) {
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
	// Get current working directory
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	// Build command arguments: -td <pid> <output_path>
	args := []string{
		"-td",
		strconv.Itoa(d.Pid),
		workDir,
	}

	// Execute the dotnet tool and capture output
	file, err := executeDotnetTool(args, fmt.Sprintf(dotnetThreadOutputPath, d.Pid))
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET thread dump: %w", err)
	}

	return file, nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "td".
func (d *DotnetThread) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "td", file)
	return Result{Msg: msg, Ok: ok}
}
