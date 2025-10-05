package capture

import (
	"fmt"
	"os"
	"strconv"
)

const dotnetThreadOutputPath = "dotnet_thread_%d.json"

// DotnetThread captures .NET thread dump.
type DotnetThread struct {
	Capture
	Pid int
}

// Run implements the capture by creating the output file, capturing thread dump,
// and then uploading the captured file.
func (d *DotnetThread) Run() (Result, error) {
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
