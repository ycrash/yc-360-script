package capture

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const (
	vmstatOutputPath = "vmstat.out"
	vmstatInterval   = "5" // Default interval in seconds
)

// VMStat handles the capture of vmstat data.
type VMStat struct {
	Capture
}

// Run executes the VMStat capture process and uploads the captured file
// to the specified endpoint.
func (v *VMStat) Run() (Result, error) {
	capturedFile, err := v.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := v.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile captures VMStat output to a file.
// It returns the file handle for the captured data.
func (v *VMStat) CaptureToFile() (*os.File, error) {
	file, err := os.Create(vmstatOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := v.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := file.Sync(); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return file, nil
}

// captureOutput handles the actual VMStat capture process.
func (v *VMStat) captureOutput(f *os.File) error {
	// The first vmstat command to try
	cmd, err := executils.VMState.AddDynamicArg(
		strconv.Itoa(executils.VMSTAT_INTERVAL),
		vmstatInterval,
	)

	if err != nil {
		return fmt.Errorf("failed to build initial command: %w", err)
	}

	if err := v.executeCommand(f, cmd); err != nil {
		if runtime.GOOS != "linux" {
			return err
		}

		// If failed, fallback to the next command
		return v.fallbackCommand(f)
	}

	return nil
}

// executeCommand starts and monitors a command writing to the specified writer.
func (v *VMStat) executeCommand(w io.Writer, cmd []string) error {
	command, err := executils.CommandStartInBackgroundToWriter(w, cmd)
	if err != nil {
		return fmt.Errorf("failed to start command: %w", err)
	}
	v.Cmd = command

	if command.IsSkipped() {
		return nil
	}

	command.Wait()

	if command.ExitCode() == 0 || runtime.GOOS != "linux" {
		return nil
	}

	return fmt.Errorf("command failed with exit code: %d", command.ExitCode())
}

// fallbackCommand attempts to run VMStat with an alternative approach when the initial command fails.
func (v *VMStat) fallbackCommand(file *os.File) error {
	// Preserve the original output for logging
	output, readErr := v.preserveOriginalOutput(file)

	cmd, err := (&executils.Command{
		executils.WaitCommand,
		executils.Executable(),
		"-vmstatMode",
		executils.DynamicArg,
		executils.DynamicArg,
		`| awk '{cmd="(date +'%H:%M:%S')"; cmd | getline now; print now $0; fflush(); close(cmd)}'`,
	}).AddDynamicArg(
		strconv.Itoa(executils.VMSTAT_INTERVAL),
		vmstatInterval,
	)
	if err != nil {
		return fmt.Errorf("failed to build fallback command: %w", err)
	}

	logger.Info().
		Strs("cmd", cmd).
		Err(readErr).
		Bytes("output", output).
		Str("failed cmd", v.Cmd.String()).
		Msg("vmstat failed, trying to use -vmstatMode")

	return v.executeCommand(file, cmd)
}

// preserveOriginalOutput saves the original command output and returns it as bytes.
func (v *VMStat) preserveOriginalOutput(file *os.File) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to start: %w", err)
	}

	output, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("failed to read original output: %w", err)
	}

	if err := file.Truncate(0); err != nil {
		return nil, fmt.Errorf("failed to truncate file: %w", err)
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("failed to seek to start after truncate: %w", err)
	}

	return output, nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (v *VMStat) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(v.Endpoint(), "vmstat", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
