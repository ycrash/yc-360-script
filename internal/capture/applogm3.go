package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/mattn/go-zglob"
)

// AppLogM3 captures incremental log changes for multiple applications,
// tracking the last-read positions per file.
type AppLogM3 struct {
	Capture

	// Paths maps a process ID to its configured log file patterns.
	Paths map[int]config.AppLogs // int=pid

	// readStats tracks the last read position and file size per file.
	readStats map[string]appLogM3ReadStat
}

// appLogM3ReadStat tracks the essential state needed for incremental log reading.
type appLogM3ReadStat struct {
	filePath string

	// fileSize captures the last known size of the file
	// This is compared against current file size to detect log rotation
	// When current size < fileSize, we assume the file was rotated
	fileSize int64

	// readPosition marks where we left off in previous read
	readPosition int64
}

func NewAppLogM3() *AppLogM3 {
	return &AppLogM3{
		readStats: make(map[string]appLogM3ReadStat),
		Paths:     make(map[int]config.AppLogs),
	}
}

func (a *AppLogM3) SetPaths(p map[int]config.AppLogs) {
	a.Paths = p
}

// Run iterates over each configured PID and its log file patterns,
// expands globs, and captures incremental log content.
//
// The method uses glob pattern expansion to support wildcard log paths,
// which is essential for handling rotating log files (e.g., app.log.1, app.log.2).
// Errors are collected but don't stop processing - this ensures one bad file
// doesn't prevent capture from other valid logs.
func (a *AppLogM3) Run() (Result, error) {
	results := []Result{}
	errs := []error{}

	// Loop over process IDs and their associated log path patterns.
	for pid, paths := range a.Paths {
		for _, path := range paths {
			matches, err := zglob.Glob(string(path))

			if err != nil {
				results = append(results, Result{
					Msg: fmt.Sprintf("invalid glob pattern %q", path),
					Ok:  false,
				})
				errs = append(errs, err)

				continue
			}

			for _, match := range matches {
				r, e := a.CaptureSingleAppLog(match, pid)

				results = append(results, r)
				errs = append(errs, e)
			}
		}
	}

	return summarizeResults(results, errs)
}

// captureSingleAppLog processes a single log file: it opens the file,
// seeks to the last-read position (or initializes it on the first run),
// copies new content to a uniquely named destination file, and posts it.
func (a *AppLogM3) CaptureSingleAppLog(filePath string, pid int) (Result, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to stat applog %q: %w", filePath, err)
	}

	src, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to open applog %q: %w", filePath, err)
	}
	defer src.Close()

	readStat, statExist := a.readStats[filePath]
	readStat.filePath = filePath

	if !statExist {
		// On first encounter, skip initial content to avoid processing potentially large historical logs.
		// Instead, set the read position to the current end of file and just return,
		// so that the next run will read from there.
		readStat.fileSize = fileInfo.Size()
		readStat.readPosition = fileInfo.Size()
		a.readStats[filePath] = readStat

		return Result{
			Msg: fmt.Sprintf("initialized read position for %q", filePath),
			Ok:  true,
		}, nil
	}

	// Detect log rotation by checking if file size decreased
	// This avoids missing logs after rotation while preventing
	// duplicate processing of log entries
	if fileInfo.Size() < readStat.fileSize {
		logger.Log("applogm3: file %q truncated, resetting read position", filePath)
		readStat.readPosition = 0
	} else {
		// Seek to last read position for incremental processing
		if _, err := src.Seek(readStat.readPosition, io.SeekStart); err != nil {
			// If seek fails, fall back to processing from start to ensure
			// no log entries are missed, even if some may be duplicated
			logger.Log("applogm3: failed to seek %q to pos %d: %v, resetting to start",
				filePath, readStat.readPosition, err)
			if _, err = src.Seek(0, io.SeekStart); err != nil {
				return Result{}, fmt.Errorf("failed to seek applog %q: %w", filePath, err)
			}
			readStat.readPosition = 0
		}
	}

	logger.Log("applogm3: reading %q from pos %d", filePath, readStat.readPosition)

	// Generate a unique destination filename to prevent conflicting file names.
	dstPath := generateUniqueLogPath(filepath.Base(filePath))
	dst, err := os.Create(dstPath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create destination file %q: %w", dstPath, err)
	}
	defer dst.Close()

	// Copy new content from the source log.
	bytesCopied, err := io.Copy(dst, src)
	if err != nil {
		return Result{}, fmt.Errorf("failed to copy content from %q: %w", filePath, err)
	}

	// Update readStats for next run
	readStat.readPosition += bytesCopied
	readStat.fileSize = fileInfo.Size()
	a.readStats[filePath] = readStat

	// Ensure all writes are flushed to disk.
	if err := dst.Sync(); err != nil {
		return Result{}, fmt.Errorf("failed to sync destination file %q: %w", dstPath, err)
	}

	// Build the data string for posting.
	dt := fmt.Sprintf("applog&logName=%s&pid=%d", filepath.Base(filePath), pid)
	msg, ok := PostData(a.Endpoint(), dt, dst)

	return Result{Msg: msg, Ok: ok}, nil
}
