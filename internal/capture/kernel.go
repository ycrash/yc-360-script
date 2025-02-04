package capture

import (
	"errors"
	"fmt"
	"io"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const kernelOutputPath = "kernel.out"

// Kernel handles the capture of kernel params information.
type Kernel struct {
	Capture
}

// Run executes the kernel capture process and uploads the captured file
// to the specified endpoint.
func (k *Kernel) Run() (Result, error) {
	if executils.KernelParam == nil {
		return Result{
			Msg: "skipped capturing Kernel",
			Ok:  false,
		}, nil
	}

	capturedFile, err := k.CaptureToFile()
	if err != nil {
		return Result{
			Msg: err.Error(),
			Ok:  false,
		}, fmt.Errorf("failed to capture kernel data: %w", err)
	}
	defer capturedFile.Close()

	result := k.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile creates a new file and captures kernel information into it.
// The function handles file creation and ensures proper cleanup in case of errors.
func (k *Kernel) CaptureToFile() (*os.File, error) {
	file, err := os.Create(kernelOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := k.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := k.syncFile(file); err != nil {
		logger.Log("warning: failed to sync kernel output file: %v", err)
	}

	return file, nil
}

// captureOutput handles the actual kernel data capture process.
// It executes the kernel capture command and writes the output to the provided file.
func (k *Kernel) captureOutput(w io.Writer) error {
	cmd, err := executils.CommandStartInBackgroundToWriter(w, executils.KernelParam)
	if err != nil {
		return fmt.Errorf("failed to start kernel capture command: %w", err)
	}
	k.Cmd = cmd

	if cmd.IsSkipped() {
		return nil
	}

	if err := cmd.Wait(); err != nil {
		logger.Log("kernel capture command failed: %v", err)
		return fmt.Errorf("kernel capture command failed: %w", err)
	}

	if cmd.ExitCode() != 0 {
		return fmt.Errorf("kernel capture command exited with code %d", cmd.ExitCode())
	}

	return nil
}

// UploadCapturedFile uploads the captured kernel data file to the configured endpoint.
// It handles the POST operation and returns a Result indicating success or failure.
func (k *Kernel) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(k.Endpoint(), "kernel", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// syncFile ensures all captured data is written to disk.
// This is important for data integrity before uploading.
func (k *Kernel) syncFile(file *os.File) error {
	err := file.Sync()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
