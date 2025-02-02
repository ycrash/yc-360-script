package capture

import (
	"errors"
	"fmt"
	"io"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const dmesgOutputPath = "dmesg.out"

// ErrNonZeroExit indicates that a command exited with a non-zero status code.
var ErrNonZeroExit = errors.New("command exited with non-zero status")

// DMesgCapture handles the capture of kernel message buffer data.
type DMesg struct {
	Capture
}

// Run executes the dmesg capture process and uploads the captured file
// to the specified endpoint.
func (d *DMesg) Run() (Result, error) {
	if executils.DMesg == nil && executils.DMesg2 == nil {
		return Result{
			Msg: "skipped capturing DMesg",
			Ok:  true,
		}, nil
	}

	capturedFile, err := d.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := d.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile captures dmesg output to a file, handling both primary and fallback commands.
// It returns the file handle for the captured data.
func (d *DMesg) CaptureToFile() (*os.File, error) {
	file, err := os.Create(dmesgOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := d.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := d.syncFile(file); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return file, nil
}

// captureOutput handles the actual capture process, attempting the primary command
// and falling back to the secondary command if needed.
func (d *DMesg) captureOutput(file *os.File) error {
	if err := d.captureWithPrimaryCommand(file); err != nil {

		// This fallback mechanism has been here since the beginning.
		// We keep the same logic with the refactor, but it seems to be not working.

		// Bug: Fallback detection is unreliable
		//
		// The fallback mechanism triggers on non-zero exit codes, but fails to detect
		// actual dmesg failures due to how pipes work. The command:
		//   "dmesg -T --level=emerg,alert,crit,err,warn | tail -20"
		// will return exit code 0 (success) even when dmesg fails, because the exit
		// code comes from 'tail' rather than 'dmesg'.
		if errors.Is(err, ErrNonZeroExit) {
			if err := d.resetFile(file); err != nil {
				return fmt.Errorf("failed to reset file for fallback: %w", err)
			}

			return d.captureWithFallbackCommand(file)
		}

		return err
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (d *DMesg) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(d.Endpoint(), "dmesg", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// runPrimaryCapture attempts to capture dmesg output using the primary command.
func (d *DMesg) captureWithPrimaryCommand(w io.Writer) error {
	cmd, err := executils.CommandStartInBackgroundToWriter(w, executils.DMesg)
	if err != nil {
		return fmt.Errorf("failed to start primary dmesg command: %w", err)
	}
	d.Cmd = cmd

	if cmd.IsSkipped() {
		return nil
	}

	if err := cmd.Wait(); err != nil {
		logger.Log("primary command failed: %v", err)
		return err
	}

	if cmd.ExitCode() != 0 {
		return ErrNonZeroExit
	}

	return nil
}

// runFallbackCapture attempts to capture dmesg output using the fallback command
// when the primary command fails.
func (d *DMesg) captureWithFallbackCommand(file *os.File) error {
	cmd, err := executils.CommandStartInBackgroundToWriter(file, executils.DMesg2)
	if err != nil {
		return fmt.Errorf("failed to start fallback dmesg command: %w", err)
	}
	d.Cmd = cmd

	if cmd.IsSkipped() {
		return nil
	}

	if err := cmd.Wait(); err != nil {
		logger.Log("fallback command failed: %v", err)
		return fmt.Errorf("fallback command failed: %w", err)
	}

	if cmd.ExitCode() != 0 {
		return ErrNonZeroExit
	}

	return nil
}

// resetFile prepares the file for reuse by the fallback command.
func (d *DMesg) resetFile(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return fmt.Errorf("failed to truncate file: %w", err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("failed to seek to start: %w", err)
	}

	return nil
}

// syncFile ensures all file data is written to disk.
func (d *DMesg) syncFile(file *os.File) error {
	err := file.Sync()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
