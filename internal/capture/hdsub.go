package capture

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const hdsubOutputPath = "hdsub.out"

// HDSub handles the capture of Java heap and VM data for a specified process.
type HDSub struct {
	Capture
	JavaHome string
	Pid      int
}

// Run executes the heap dump capture process and uploads the captured file
// to the specified endpoint.
func (t *HDSub) Run() (Result, error) {
	capturedFile, err := t.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := t.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile captures Java heap and VM data to a file.
// It returns the file handle for the captured data.
func (t *HDSub) CaptureToFile() (*os.File, error) {
	file, err := os.Create(hdsubOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	// Capture each section of data
	if err := t.captureClassHistogram(file); err != nil {
		logger.Log("Failed to capture class histogram: %v", err)
	}

	if err := t.captureSystemProperties(file); err != nil {
		logger.Log("Failed to capture system properties: %v", err)
	}

	if err := t.captureHeapInfo(file); err != nil {
		logger.Log("Failed to capture heap info: %v", err)
	}

	if err := t.captureVMFlags(file); err != nil {
		logger.Log("Failed to capture VM flags: %v", err)
	}

	if err := t.syncFile(file); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return file, nil
}

// captureClassHistogram captures GC.class_histogram data to the writer.
func (t *HDSub) captureClassHistogram(w io.Writer) error {
	if _, err := w.Write([]byte("GC.class_histogram:\n")); err != nil {
		return fmt.Errorf("failed to write section header: %w", err)
	}

	return t.executeJcmd(w, "GC.class_histogram")
}

// captureSystemProperties captures VM.system_properties data to the writer.
func (t *HDSub) captureSystemProperties(w io.Writer) error {
	if _, err := w.Write([]byte("\nVM.system_properties:\n")); err != nil {
		return fmt.Errorf("failed to write section header: %w", err)
	}

	return t.executeJcmd(w, "VM.system_properties")
}

// captureHeapInfo captures GC.heap_info data to the writer.
func (t *HDSub) captureHeapInfo(w io.Writer) error {
	if _, err := w.Write([]byte("\nGC.heap_info:\n")); err != nil {
		return fmt.Errorf("failed to write section header: %w", err)
	}

	return t.executeJcmd(w, "GC.heap_info")
}

// captureVMFlags captures VM.flags data to the writer.
func (t *HDSub) captureVMFlags(w io.Writer) error {
	if _, err := w.Write([]byte("\nVM.flags:\n")); err != nil {
		return fmt.Errorf("failed to write section header: %w", err)
	}

	return t.executeJcmd(w, "VM.flags")
}

// executeJcmd executes the jcmd command with the given parameters, falling back to
// jattach if needed.
func (t *HDSub) executeJcmd(w io.Writer, command string) error {
	// Try using jcmd first
	err := executils.CommandCombinedOutputToWriter(w,
		executils.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), command},
		executils.SudoHooker{PID: t.Pid})

	if err == nil {
		return nil
	}

	logger.Log("Failed to run jcmd with err %v. Trying to capture using jattach...", err)

	// Try using jattach as fallback
	err = executils.CommandCombinedOutputToWriter(w,
		executils.Command{executils.Executable(), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(t.Pid)},
		executils.SudoHooker{PID: t.Pid})

	if err == nil {
		return nil
	}

	logger.Log("Failed to capture %s with err %v. Trying to capture using tmp jattach...", command, err)

	// Try using temp jattach as last resort
	tempPath, err := executils.Copy2TempPath()
	if err != nil {
		return fmt.Errorf("failed to create temp jattach: %w", err)
	}

	err = executils.CommandCombinedOutputToWriter(w,
		executils.Command{tempPath, "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(t.Pid)},
		executils.SudoHooker{PID: t.Pid})

	if err != nil {
		return fmt.Errorf("failed to capture %s: %w", command, err)
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (t *HDSub) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(t.Endpoint(), "hdsub", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// syncFile ensures all file data is written to disk.
func (t *HDSub) syncFile(file *os.File) error {
	err := file.Sync()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
