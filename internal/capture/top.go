package capture

import (
	"fmt"
	"io"
	"os"
	"strconv"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const topOutputPath = "top.out"

// Top captures system "top" data.
type Top struct {
	Capture
}

// Run implements the capture by creating the output file, capturing output,
// and then uploading the captured file.
func (t *Top) Run() (Result, error) {
	// If the primary top command isn’t configured, skip capturing.
	if len(executils.Top) == 0 {
		return Result{Msg: "skipped capturing Top", Ok: false}, nil
	}

	capturedFile, err := t.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	return t.UploadCapturedFile(capturedFile), nil
}

// CaptureToFile captures the ps to a file and returns it
func (t *Top) CaptureToFile() (*os.File, error) {
	file, err := os.Create(topOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := t.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := file.Sync(); err != nil {
		logger.Log("failed to sync file: %v", err)
	}

	return file, nil
}

// captureOutput executes the primary top command. If that fails and a fallback
// command is available, it resets the file and retries.
func (t *Top) captureOutput(f *os.File) error {
	// Try the primary top command.
	var err error
	t.Cmd, err = executils.CommandStartInBackgroundToWriter(f, executils.Top)
	if err != nil {
		return err
	}

	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}

	// If a fallback exists, try it.
	if t.Cmd.ExitCode() != 0 && executils.Top2 != nil && len(executils.Top2) > 0 {
		// Reset file before retrying.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return err
		}

		logger.Log("primary top command failed, trying fallback: %v", executils.Top2)
		t.Cmd, err = executils.CommandStartInBackgroundToWriter(f, executils.Top2)
		if err != nil {
			return err
		}

		err = t.Cmd.Wait()
		if err != nil {
			return fmt.Errorf("both top commands failed: %w", err)
		}
	} else {
		return err
	}

	return nil
}

// UploadCapturedFile sends the file data to the endpoint using the service key "top".
func (t *Top) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(t.Endpoint(), "top", file)
	return Result{Msg: msg, Ok: ok}
}

// TopH captures "top -H" (threads) data for a specific process.
type TopH struct {
	Capture
	Pid int
	N   int // used to distinguish output files (e.g. topdashH.1.out, topdashH.2.out, …)
}

// Run captures the "top -H "output (with fallback if needed)
// and then returns a Result.
// (Note that unlike Top, TopH does not upload the captured file.)
func (t *TopH) Run() (Result, error) {
	// If the primary topH command isn’t configured, skip capturing.
	if len(executils.TopH) == 0 {
		return Result{Msg: "skipped capturing TopH", Ok: false}, nil
	}

	// Check that the process exists.
	if !IsProcessExists(t.Pid) {
		return Result{}, fmt.Errorf("process %d does not exist", t.Pid)
	}

	logger.Log("Collection of top dash H data started for PID %d.", t.Pid)

	capturedFile, err := t.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	// In the original implementation the file was not uploaded.
	// Return a success result indicating that the file was created.
	return Result{Msg: fmt.Sprintf("captured top dash H data to %s", capturedFile.Name()), Ok: true}, nil
}

// CaptureToFile creates an output file named "topdashH.<N>.out", writes the
// command output into it (with fallback if needed), syncs the file and returns it.
func (t *TopH) CaptureToFile() (*os.File, error) {
	fileName := fmt.Sprintf("topdashH.%d.out", t.N)
	file, err := os.Create(fileName)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := t.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := file.Sync(); err != nil {
		logger.Log("failed to sync file: %v", err)
	}
	return file, nil
}

// captureOutput builds and executes the primary topH command (adding the dynamic
// PID argument). If that fails and a fallback command is available, it resets the file and retries.
func (t *TopH) captureOutput(f *os.File) error {
	// Build the primary command with the process PID.
	command, err := executils.TopH.AddDynamicArg(strconv.Itoa(t.Pid))
	if err != nil {
		return err
	}

	t.Cmd, err = executils.CommandStartInBackgroundToWriter(f, command)
	if err != nil {
		return err
	}

	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}

	// If a fallback exists, try it.
	if t.Cmd.ExitCode() != 0 && executils.TopH2 != nil && len(executils.TopH2) > 0 {
		// Reset write position
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := f.Truncate(0); err != nil {
			return err
		}

		logger.Log("primary top dash H command failed, trying fallback: %v", executils.TopH2)

		// Append the PID to the fallback command.
		fallbackCmd := executils.Append(executils.TopH2, strconv.Itoa(t.Pid))

		t.Cmd, err = executils.CommandStartInBackgroundToWriter(f, fallbackCmd)
		if err != nil {
			return err
		}

		err = t.Cmd.Wait()
		if err != nil {
			return fmt.Errorf("both top dash H commands failed: %w", err)
		}
	} else {
		return err
	}

	return nil
}
