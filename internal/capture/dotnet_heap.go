package capture

import (
	"fmt"
	"os"
	"strconv"
)

const dotnetHeapDumpOutputPath = "heap_dump.dmp"

// DotnetHeapDump captures .NET full heap dump.
type DotnetHeapDump struct {
	Capture
	Pid int
}

// Run implements the capture by creating the output file, capturing full heap dump,
// and then uploading the captured file.
func (d *DotnetHeapDump) Run() (Result, error) {
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

// CaptureToFile captures the full heap dump to a file and returns it.
func (d *DotnetHeapDump) CaptureToFile() (*os.File, error) {
	// Get current working directory
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	// Build command arguments: -hd <pid> <output_folder>
	// TODO: pass the optional 4th argument (procdump_timeout_seconds); until then the
	// helper falls back to YC_PROCDUMP_TIMEOUT_SECONDS or its own 1800s default.
	args := []string{
		"-hd",
		strconv.Itoa(d.Pid),
		workDir,
	}

	// Execute the dotnet tool and capture output. ProcDump resolution happens inside
	// the helper; see config.FindProcDump for the startup preflight.
	file, err := executeDotnetTool(d.Pid, args, dotnetHeapDumpOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to capture .NET heap dump: %w", err)
	}

	return file, nil
}

// UploadCapturedFile streams the raw .dmp to the heap receiver as Content-Encoding=zst,
// or skips the upload in only-capture mode (the .dmp is left on disk for the bundle).
func (d *DotnetHeapDump) UploadCapturedFile(file *os.File) Result {
	return uploadCapturedFileWithZstdCompression(d.Endpoint(), "hd", file)
}
