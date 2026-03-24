package capture

import (
	"fmt"
	"os"
	"strconv"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

const dotnetGCOutputPath = "gc_output_%d.json"

// DotnetGC captures .NET garbage collection events.
type DotnetGC struct {
	Capture
	Pid          int
	Duration     int    // Duration in seconds for GC capture
	AsyncLogPath string // If set, snapshot and upload this file instead of running a new capture
}

// Run implements the capture by creating the output file, capturing GC events,
// and then uploading the captured file. When AsyncLogPath is set, it snapshots
// and uploads the accumulated async GC log instead of spawning a fresh capture.
func (d *DotnetGC) Run() (Result, error) {
	if d.AsyncLogPath != "" {
		return d.uploadAsyncLog()
	}

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

// uploadAsyncLog snapshots the accumulated async GC log and uploads it.
// DotnetGCLog.Copy streams all complete event lines, bounded to the file size
// at call time — safe while the async collector is still appending.
func (d *DotnetGC) uploadAsyncLog() (Result, error) {
	logger.Log("using async gc snapshot for incident capture pid=%d source=%s", d.Pid, d.AsyncLogPath)

	gcLog, err := OpenDotnetGCLog(d.AsyncLogPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed to open async gc log %s: %s", d.AsyncLogPath, err), Ok: false}, err
	}
	defer gcLog.Close()

	snapshotPath := fmt.Sprintf(dotnetGCOutputPath, d.Pid)
	snapshotFile, err := os.Create(snapshotPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed to create gc snapshot %s: %s", snapshotPath, err), Ok: false}, err
	}
	defer snapshotFile.Close()

	if err = gcLog.Copy(snapshotFile); err != nil {
		return Result{Msg: fmt.Sprintf("failed to snapshot gc events %s: %s", snapshotPath, err), Ok: false}, err
	}

	if err = snapshotFile.Sync(); err != nil {
		return Result{Msg: fmt.Sprintf("failed to sync gc snapshot %s: %s", snapshotPath, err), Ok: false}, err
	}
	if _, err = snapshotFile.Seek(0, 0); err != nil {
		return Result{Msg: fmt.Sprintf("failed to rewind gc snapshot %s: %s", snapshotPath, err), Ok: false}, err
	}

	return d.UploadCapturedFile(snapshotFile), nil
}

// CaptureToFile captures the GC events to a file and returns it.
func (d *DotnetGC) CaptureToFile() (*os.File, error) {
	// Get current working directory
	workDir, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("failed to get working directory: %w", err)
	}

	d.Duration = int(config.GlobalConfig.GcDuration)
	if d.Duration == 0 {
		d.Duration = 30 // if duration 0, then set it to 30 seconds (default)
	}

	logger.Log(".net gc duration %d", d.Duration)
	// Build command arguments: -gc <pid> <output_path> duration
	args := []string{
		"-gc",
		strconv.Itoa(d.Pid),
		workDir,
		strconv.Itoa(d.Duration),
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
