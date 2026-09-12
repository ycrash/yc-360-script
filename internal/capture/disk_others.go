//go:build !windows

package capture

import (
	"fmt"
	"os"

	"yc-agent/internal/capture/executils"
)

// CaptureToFile executes the disk metrics collection command and saves output to a file.
func (d *Disk) CaptureToFile() (*os.File, error) {
	file, err := executils.CommandCombinedOutputToFile(outputFile, executils.Disk)
	if err != nil {
		return nil, fmt.Errorf("failed to execute disk command: %w", err)
	}

	return file, nil
}
