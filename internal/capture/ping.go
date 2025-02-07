package capture

import (
	"errors"
	"fmt"
	"io"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const pingOutputPath = "ping.out"

// Ping handles the capture of network ping data for a specified host.
type Ping struct {
	Capture
	Host string
}

// Run executes the ping capture process and uploads the captured file
// to the specified endpoint.
func (p *Ping) Run() (Result, error) {
	if executils.Ping == nil {
		return Result{
			Msg: "skipped capturing Ping",
			Ok:  true,
		}, nil
	}

	capturedFile, err := p.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer capturedFile.Close()

	result := p.UploadCapturedFile(capturedFile)
	return result, nil
}

// CaptureToFile captures ping output to a file.
// It returns the file handle for the captured data.
func (p *Ping) CaptureToFile() (*os.File, error) {
	file, err := os.Create(pingOutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := p.captureOutput(file); err != nil {
		file.Close()
		return nil, err
	}

	if err := p.syncFile(file); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	return file, nil
}

// captureOutput handles the actual ping capture process.
func (p *Ping) captureOutput(w io.Writer) error {
	cmd, err := executils.CommandStartInBackgroundToWriter(w, executils.Append(executils.Ping, p.Host))
	if err != nil {
		return fmt.Errorf("failed to start ping command: %w", err)
	}
	p.Cmd = cmd

	if cmd.IsSkipped() {
		return nil
	}

	if err := cmd.Wait(); err != nil {
		logger.Log("ping command failed: %v", err)
		return err
	}

	if cmd.ExitCode() != 0 {
		return ErrNonZeroExit
	}

	return nil
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (p *Ping) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(p.Endpoint(), "ping", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// syncFile ensures all file data is written to disk.
func (p *Ping) syncFile(file *os.File) error {
	err := file.Sync()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}
	return nil
}
