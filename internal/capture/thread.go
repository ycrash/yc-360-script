package capture

import (
	"fmt"
	"io"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const tdOut = "threaddump.out"

// ThreadDump represents configuration for capturing a Java thread dump.
// It can either copy an existing thread dump file or capture a new one
// from a running Java process.
type ThreadDump struct {
	Capture
	Pid      int    // Process ID of the target Java process
	TdPath   string // Path to an existing thread dump file
	JavaHome string
}

// Run executes the thread dump capture and uploads the captured file
// to the specified endpoint.
func (t *ThreadDump) Run() (Result, error) {
	capturedFile, err := t.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := t.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile attempts to obtain a thread dump either by copying an existing file
// or by capturing from a running process. It returns the file containing the thread dump.
func (t *ThreadDump) CaptureToFile() (*os.File, error) {
	// Try copying existing thread dump file if path is provided
	if t.TdPath != "" {
		file, err := t.copyThreadDumpFile()
		if err == nil {
			return file, nil
		}
		logger.Log("failed to copy thread dump from %q: %v", t.TdPath, err)
	}

	// Fall back to capturing from process if valid PID is provided
	if t.Pid > 0 {
		return t.captureFromProcess()
	}

	return nil, fmt.Errorf("no valid thread dump source: requires either TdPath or valid Pid")
}

// UploadCapturedFile uploads the thread dump file to the configured endpoint.
func (t *ThreadDump) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(t.Endpoint(), "td", file)
	return Result{Msg: msg, Ok: ok}
}

// copyThreadDumpFile copies an existing thread dump file to the output location.
func (t *ThreadDump) copyThreadDumpFile() (*os.File, error) {
	srcFile, err := os.Open(t.TdPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open source file %q: %w", t.TdPath, err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(tdOut)
	if err != nil {
		return nil, fmt.Errorf("failed to create destination file %q: %w", tdOut, err)
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		return nil, fmt.Errorf("failed to copy from %q to %q: %w", t.TdPath, tdOut, err)
	}

	if _, err := dstFile.Seek(0, io.SeekStart); err != nil {
		dstFile.Close()
		return nil, fmt.Errorf("failed to rewind destination file %q: %w", tdOut, err)
	}

	return dstFile, nil
}

// captureFromProcess captures a thread dump from a running Java process.
func (t *ThreadDump) captureFromProcess() (*os.File, error) {
	if !IsProcessExists(t.Pid) {
		return nil, fmt.Errorf("process %d does not exist", t.Pid)
	}

	logger.Log("Collecting thread dump using JStack...")
	jstack := NewJStack(t.JavaHome, t.Pid)
	if _, err := jstack.Run(); err != nil {
		logger.Log("jstack error: %v", err)
	} else {
		logger.Log("Collected thread dump...")
	}

	if err := executils.CommandRun(executils.AppendJavaCoreFiles); err != nil {
		return nil, err
	}

	// In order to be valid, it should run after TopH
	// TODO(Andy): This order dependency with TopH is hidden;
	// it's not a good design, we should refactor this later.
	if err := executils.CommandRun(executils.AppendTopHFiles); err != nil {
		return nil, err
	}

	return os.Open(tdOut)
}
