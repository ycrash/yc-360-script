package capture

import (
	"fmt"
	"os"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const outputFile = "disk.out"

// Disk represents a disk metrics collector.
// It gathers disk usage statistics and uploads them to a specified endpoint.
type Disk struct {
	Capture

	// sleepBetweenCaptures, when set, appends a second reading that far after the
	// first, into the same file. Zero takes one reading, which is what every
	// application capture does. A pair is what turns "the disk is 91% full" into
	// "the disk filled by 4 points while we watched".
	sleepBetweenCaptures time.Duration

	// stop cuts the gap short when the run is torn down; nil waits it out.
	stop <-chan struct{}
}

// Run collects and uploads the disk metrics collection.
func (d *Disk) Run() (Result, error) {
	file, err := d.CaptureToFile()
	if err != nil {
		return Result{}, fmt.Errorf("failed to capture disk metrics: %w", err)
	}
	defer file.Close()

	// A failed second reading keeps the first: it is still the better half of the
	// answer, and losing it would be worse than reporting one edge.
	if err := d.captureSecondReading(file); err != nil {
		logger.Log("warning: failed to take the second disk reading: %v", err)
	}

	return d.UploadCapturedFile(file)
}

// captureSecondReading appends a timestamped second reading. The timestamp is
// written only here, so single-reading output stays byte-identical to before.
func (d *Disk) captureSecondReading(file *os.File) error {
	if d.sleepBetweenCaptures <= 0 {
		return nil
	}

	if !snapshotGapElapsed(d.sleepBetweenCaptures, d.stop) {
		return nil
	}

	if _, err := fmt.Fprintf(file, "\n%s\n", executils.NowString()); err != nil {
		return fmt.Errorf("failed to write reading separator: %w", err)
	}

	if err := executils.CommandCombinedOutputToWriter(file, executils.Disk); err != nil {
		return fmt.Errorf("failed to execute disk command: %w", err)
	}

	if err := file.Sync(); err != nil {
		logger.Log("warning: failed to sync disk output file: %v", err)
	}

	return nil
}

// CaptureToFile executes the disk metrics collection command and saves output to a file.
func (d *Disk) CaptureToFile() (*os.File, error) {
	file, err := executils.CommandCombinedOutputToFile(outputFile, executils.Disk)
	if err != nil {
		return nil, fmt.Errorf("failed to execute disk command: %w", err)
	}

	return file, nil
}

// UploadCapturedFile sends the collected disk metrics to the configured endpoint.
func (d *Disk) UploadCapturedFile(file *os.File) (Result, error) {
	msg, ok := PostData(d.endpoint, "df", file)

	return Result{
		Msg: msg,
		Ok:  ok,
	}, nil
}
