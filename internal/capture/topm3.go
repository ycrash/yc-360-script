package capture

import (
	"fmt"
	"io"
	"os"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const top4m3OutputPath = "top4m3.out"

const defaultSleepBetweenTop4M3Capture = 20 * time.Second

// Top4M3 captures top output in 3 rounds and then uploads the file.
type Top4M3 struct {
	Capture
	sleepBetweenCaptures time.Duration // Can be adjusted to a small duration to speed up testing
}

// Run is the main entry point for capturing Top4M3 data.
// It creates an output file, writes three rounds of data with delays,
// and then uploads the captured file.
func (t *Top4M3) Run() (Result, error) {
	// If the command is not available, skip capturing.
	if len(executils.Top4M3) < 1 {
		return Result{
			Msg: "skipped capturing Top4M3",
			Ok:  false,
		}, nil
	}

	capturedFile, err := t.captureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	if err := capturedFile.Sync(); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return t.UploadCapturedFile(capturedFile), nil
}

// captureToFile creates the output file and writes the captured data to it.
func (t *Top4M3) captureToFile() (*os.File, error) {
	file, err := os.Create(top4m3OutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := t.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	return file, nil
}

// captureOutput runs the top capture command 3 times, separated by line breaks
// and delays, writing the output into the provided writer.
func (t *Top4M3) captureOutput(w io.Writer) error {
	iterations := 3
	if t.sleepBetweenCaptures == 0 {
		t.sleepBetweenCaptures = defaultSleepBetweenTop4M3Capture
	}

	for i := 0; i < iterations; i++ {
		cmd, err := executils.CommandStartInBackgroundToWriter(w, executils.Top4M3)
		if err != nil {
			return fmt.Errorf("failed to start top command: %w", err)
		}
		t.Cmd = cmd

		if cmd.IsSkipped() {
			return nil
		}

		if err := cmd.Wait(); err != nil {
			logger.Log("top command failed: %v", err)
		}

		if _, err := w.Write([]byte("\n\n\n")); err != nil {
			logger.Log("failed to insert line breaks: %v", err)
		}

		// Do not sleep after the last iteration.
		if i < iterations-1 {
			time.Sleep(t.sleepBetweenCaptures)
		}
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (t *Top4M3) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(t.Endpoint(), "top", file)
	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
