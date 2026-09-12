//go:build windows

package capture

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"

	"yc-agent/internal/logger"
)

// CaptureToFile collects the logical disk usage table and saves it to a file.
//
// Was `wmic logicaldisk get size,freespace,caption`, which Windows 11
// 24H2/25H2 no longer ships. The Win32 API gives the same values; the bytes
// written stay identical to wmic's - see formatWMICTable.
func (d *Disk) CaptureToFile() (*os.File, error) {
	disks, err := logicalDisks()
	if err != nil {
		return nil, fmt.Errorf("failed to collect disk metrics: %w", err)
	}

	file, err := os.Create(outputFile)
	if err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", outputFile, err)
	}

	if _, err := file.Write(formatLogicalDiskTable(disks)); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("failed to write %s: %w", outputFile, err)
	}

	return file, nil
}

// logicalDisks returns one entry per logical drive, as Win32_LogicalDisk
// reported to wmic: every drive letter, including one whose size cannot be
// read, listed with empty size columns rather than dropped.
//
// That is why it enumerates drives itself rather than using gopsutil's
// disk.Partitions, which omits an optical or removable drive holding no media
// - changing both the row count and the column widths.
func logicalDisks() ([]logicalDisk, error) {
	roots, err := driveRoots()
	if err != nil {
		return nil, err
	}

	disks := make([]logicalDisk, 0, len(roots))
	for _, root := range roots {
		// wmic's Caption had no trailing separator: "C:", not "C:\".
		disk := logicalDisk{Caption: strings.TrimSuffix(root, `\`)}

		free, size, err := driveUsage(root)
		if err != nil {
			// wmic still listed a drive it could not measure, with both size
			// columns blank, so they are left empty here.
			logger.Debug().Err(err).Str("drive", disk.Caption).
				Msg("Drive reports no usage; listing it with empty size columns")
		} else {
			disk.FreeSpace = strconv.FormatUint(free, 10)
			disk.Size = strconv.FormatUint(size, 10)
		}

		disks = append(disks, disk)
	}

	return disks, nil
}

// driveRoots returns the root path of every logical drive ("C:\", "D:\", ...)
// in the order Windows reports them, which is alphabetical - the order wmic
// listed them in.
func driveRoots() ([]string, error) {
	// 26 drives x 4 UTF-16 units ("C:\" plus its NUL) plus a final NUL fits
	// comfortably; the call below reports if it does not.
	buf := make([]uint16, 128)

	n, err := windows.GetLogicalDriveStrings(uint32(len(buf)), &buf[0])
	if err != nil {
		return nil, fmt.Errorf("failed to list logical drives: %w", err)
	}
	if n > uint32(len(buf)) {
		buf = make([]uint16, n)
		if n, err = windows.GetLogicalDriveStrings(n, &buf[0]); err != nil {
			return nil, fmt.Errorf("failed to list logical drives: %w", err)
		}
	}

	return splitNULTerminated(buf[:n]), nil
}

// splitNULTerminated splits the NUL-separated, NUL-terminated UTF-16 list that
// GetLogicalDriveStrings writes into its buffer.
func splitNULTerminated(buf []uint16) []string {
	var out []string

	start := 0
	for i, c := range buf {
		if c != 0 {
			continue
		}
		if i > start {
			out = append(out, windows.UTF16ToString(buf[start:i]))
		}
		start = i + 1
	}

	return out
}

// driveUsage returns the free and total bytes of a drive, which are the
// FreeSpace and Size that Win32_LogicalDisk reported to wmic.
func driveUsage(root string) (free, size uint64, err error) {
	rootPtr, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return 0, 0, err
	}

	// freeToCaller is the caller's quota-limited share; wmic reported the
	// volume-wide totalFree. Equal unless disk quotas are in force, and the
	// differential tests assert it against live wmic.
	var freeToCaller, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(rootPtr, &freeToCaller, &total, &totalFree); err != nil {
		return 0, 0, err
	}

	return totalFree, total, nil
}
