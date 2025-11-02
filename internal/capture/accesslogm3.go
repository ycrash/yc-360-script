package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/mattn/go-zglob"
)

// AccessLogM3 captures incremental access log changes for multiple applications,
// tracking the last-read positions per file.
type AccessLogM3 struct {
	Capture

	// Paths maps a process ID to its configured access log entries.
	Paths map[int][]AccessLogEntry // int=pid

	// readStats tracks the last read position and file size per file.
	readStats map[string]accessLogM3ReadStat
}

// AccessLogEntry combines a path and format for access log configuration
type AccessLogEntry struct {
	Path   config.AccessLogPath
	Format config.AccessLogFormat
	Source config.AccessLogSource
}

// accessLogM3ReadStat tracks the essential state needed for incremental log reading.
type accessLogM3ReadStat struct {
	filePath string

	// fileSize captures the last known size of the file
	// This is compared against current file size to detect log rotation
	// When current size < fileSize, we assume the file was rotated
	fileSize int64

	// readPosition marks where we left off in previous read
	readPosition int64
}

func NewAccessLogM3() *AccessLogM3 {
	return &AccessLogM3{
		readStats: make(map[string]accessLogM3ReadStat),
		Paths:     make(map[int][]AccessLogEntry),
	}
}

func (a *AccessLogM3) SetPaths(p map[int][]AccessLogEntry) {
	a.Paths = p
}

// Run iterates over each configured PID and its log file patterns,
// expands globs, and captures incremental log content.
//
// The method uses glob pattern expansion to support wildcard log paths,
// which is essential for handling rotating log files (e.g., access.log.1, access.log.2).
// Errors are collected but don't stop processing - this ensures one bad file
// doesn't prevent capture from other valid logs.
func (a *AccessLogM3) Run() (Result, error) {
	results := []Result{}
	errs := []error{}

	now := time.Now()
	currentDateStr := now.Format("2006-01-02")

	// Loop over process IDs and their associated log path patterns.
	for pid, paths := range a.Paths {
		for _, path := range paths {
			// Substitute variable
			// %t = current date
			p := strings.ReplaceAll(string(path.Path), "%t", currentDateStr)

			matches, err := zglob.Glob(string(p))

			if err != nil {
				results = append(results, Result{
					Msg: fmt.Sprintf("invalid glob pattern %q", path),
					Ok:  false,
				})
				errs = append(errs, err)

				continue
			}

			for _, match := range matches {
				r, e := a.CaptureSingleAccessLog(match, pid, string(path.Format), string(path.Source))

				results = append(results, r)
				errs = append(errs, e)
			}
		}
	}

	return summarizeResults(results, errs)
}

// captureSingleAccessLog processes a single access log file: it opens the file,
// seeks to the last-read position (or initializes it on the first run),
// copies new content to a uniquely named destination file, and posts it.
func (a *AccessLogM3) CaptureSingleAccessLog(filePath string, pid int, format, source string) (Result, error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to stat accesslog %q: %w", filePath, err)
	}

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

	src, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to open accesslog %q: %w", filePath, err)
	}
	defer src.Close()

	// Detect log rotation by checking if file size decreased
	// This avoids missing logs after rotation while preventing
	// duplicate processing of log entries
	if fileInfo.Size() < readStat.fileSize {
		logger.Log("accesslogm3: file %q truncated, resetting read position", filePath)
		readStat.readPosition = 0
	} else {
		// Seek to last read position for incremental processing
		if _, err := src.Seek(readStat.readPosition, io.SeekStart); err != nil {
			// If seek fails, fall back to processing from start to ensure
			// no log entries are missed, even if some may be duplicated
			logger.Log("accesslogm3: failed to seek %q to pos %d: %v, resetting to start",
				filePath, readStat.readPosition, err)
			if _, err = src.Seek(0, io.SeekStart); err != nil {
				return Result{}, fmt.Errorf("failed to seek accesslog %q: %w", filePath, err)
			}
			readStat.readPosition = 0
		}
	}

	logger.Log("accesslogm3: reading %q from pos %d", filePath, readStat.readPosition)

	// Generate a unique destination filename to prevent conflicting file names.
	dstPath := generateUniqueAccessLogPath(filepath.Base(filePath))
	dst, err := os.Create(dstPath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create destination file %q: %w", dstPath, err)
	}
	defer dst.Close()

	// Write the format header to the destination file)
	dst.WriteString(format + "\n")

	if source != "" {
		// Write the source header
		dst.WriteString("accessLogSource: " + source + "\n")
	}

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
	dt := fmt.Sprintf("accessLog&logName=%s&pid=%d", filepath.Base(filePath), pid)
	msg, ok := PostData(a.Endpoint(), dt, dst)

	return Result{Msg: msg, Ok: ok}, nil
}

// generateUniqueAccessLogPath creates a unique file path for storing the log content.
// It appends a sequential number to the base filename until it finds an unused path.
// Returns the generated unique path as a string.
func generateUniqueAccessLogPath(baseFileName string) string {
	counter := 1
	for {
		// Generate a unique filename by appending the sequential number
		// Example: 1.accessLogs.abc.log
		path := fmt.Sprintf("%d.accessLogs.%s", counter, baseFileName)
		if !fileExists(path) {
			return path
		}

		// Keep trying until we find an available filename
		counter++
	}
}
