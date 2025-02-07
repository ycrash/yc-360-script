package capture

import (
	"errors"
	"fmt"
	"os"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const netStatOutputPath = "netstat.out"

const defaultSleepBetweenCaptures = 3 * time.Second

// NetStat captures netstat data.
type NetStat struct {
	Capture
	sleepBetweenCaptures time.Duration
	file                 *os.File
}

// Run captures netstat data twice with a delay between captures, then upload it to the specified endpoint.
func (ns *NetStat) Run() (Result, error) {
	if ns.sleepBetweenCaptures == 0 {
		ns.sleepBetweenCaptures = defaultSleepBetweenCaptures
	}

	// Create the output file.
	file, err := os.Create(netStatOutputPath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create output file: %w", err)
	}

	ns.file = file
	defer ns.close()

	// First capture.
	logger.Log("Collecting the first netstat snapshot...")
	if err := ns.CaptureToFile(); err != nil {
		// Continue execution even if first capture fails.
		logger.Log("warning: failed run first netstat capture: %v", err)
	} else {
		logger.Log("First netstat snapshot complete.")
	}

	// Wait between captures
	time.Sleep(ns.sleepBetweenCaptures)

	// New line separator between captures
	if _, err := ns.file.WriteString("\n"); err != nil {
		return Result{}, fmt.Errorf("failed to write capture separator: %w", err)
	}

	// Second capture to detect any changes from the first one.
	logger.Log("Collecting the final netstat snapshot...")
	if err := ns.CaptureToFile(); err != nil {
		logger.Log("warning: failed run second netstat capture: %v", err)
	} else {
		logger.Log("Final netstat snapshot complete.")
	}

	// Ensure data is flushed / written to the disk before upload
	if err := ns.syncFile(ns.file); err != nil {
		logger.Log("warning: failed to sync netstat output file: %v", err)
	}

	result := ns.UploadCapturedFile(ns.file)

	return result, nil
}

// CaptureToFile takes a snapshot of the netstat data and writes it to the output file.
// Multiple captures can be taken - each capture is appended in the output with a header string.
func (ns *NetStat) CaptureToFile() error {
	// Guard against usage after close or before initialization.
	if ns.file == nil {
		return fmt.Errorf("netstat is not initialized or already closed")
	}

	// Headers separate captures to make the output parseable.
	header := fmt.Sprintf("%s\n", executils.NowString())
	if _, err := ns.file.WriteString(header); err != nil {
		return fmt.Errorf("failed to write capture header: %w", err)
	}

	// Fallback to alternative netstat implementation if the primary method fails.
	err := executils.CommandCombinedOutputToWriter(ns.file, executils.NetState)
	if err != nil {
		err = netStat(true, true, true, true, false, true, false, ns.file)
		if err != nil {
			return fmt.Errorf("failed to capture netstat: %w", err)
		}
	}

	return nil
}

// UploadCapturedFile sends the captured netstat data to a remote endpoint.
func (ns *NetStat) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(ns.Endpoint(), "ns", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}

// syncFile ensures all captured data is written to disk before proceeding.
func (ns *NetStat) syncFile(file *os.File) error {
	err := file.Sync()
	if err != nil && !errors.Is(err, os.ErrClosed) {
		return err
	}

	return nil
}

// close handles cleanup of file resources.
func (ns *NetStat) close() error {
	if ns.file != nil {
		err := ns.file.Close()
		ns.file = nil
		return err
	}

	return nil
}
