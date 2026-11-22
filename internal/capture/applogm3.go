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

	// identity is the file's stat at the previous run, compared with os.SameFile to spot rotation.
	identity os.FileInfo

	// fileSize is the size at the previous run: smaller now means truncated, the same means idle.
	fileSize int64

	// readPosition is where the next upload starts; it stays before an unfinished last line.
	readPosition int64
}

// appLogM3MaxHeldBack caps how much of an unfinished last line waits for the next run.
const appLogM3MaxHeldBack = 1 << 20

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

			// A missing file is reported in words the user can act on instead
			// of as a glob error. It is a configuration mistake, not a capture
			// failure, so it goes in the result message only: returned as an
			// error, WrapRun would log a dump of this whole task every M3 cycle.
			if problem := logPathProblem(appLogLabel, string(path), err); problem != nil {
				results = append(results, Result{
					Msg: problem.Error(),
					Ok:  false,
				})
				errs = append(errs, nil)

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

// CaptureSingleAppLog processes a single log file: it opens the file,
// reads from the last-read position (or initializes it on the first run),
// copies the new complete lines to a uniquely named destination file, and posts it.
func (a *AppLogM3) CaptureSingleAppLog(filePath string, pid int) (Result, error) {
	src, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to open applog %q: %w", filePath, err)
	}
	defer src.Close()

	// Stat the open file, so the size and identity are of the file being read.
	fileInfo, err := src.Stat()
	if err != nil {
		return Result{}, fmt.Errorf("failed to stat applog %q: %w", filePath, err)
	}

	size := fileInfo.Size()
	readStat, statExist := a.readStats[filePath]
	readStat.filePath = filePath

	if !statExist {
		// On first encounter, skip the existing content up to its last complete line.
		start, err := appLogM3UploadEnd(src, 0, size, true)
		if err != nil {
			return Result{}, fmt.Errorf("failed to scan applog %q: %w", filePath, err)
		}
		readStat.identity = fileInfo
		readStat.fileSize = size
		readStat.readPosition = start
		a.readStats[filePath] = readStat

		return Result{
			Msg: fmt.Sprintf("initialized read position for %q", filePath),
			Ok:  true,
		}, nil
	}

	// A different file at the path means rotation; the same file getting smaller means copytruncate.
	replaced := !os.SameFile(readStat.identity, fileInfo)
	switch {
	case replaced:
		logger.Log("applogm3: file %q was replaced, reading the new file from the start", filePath)
		readStat.readPosition = 0
	case size < readStat.fileSize:
		logger.Log("applogm3: file %q truncated, resetting read position", filePath)
		readStat.readPosition = 0
	}

	// An unfinished last line waits for the next run, unless the file has stopped growing.
	start := readStat.readPosition
	end, err := appLogM3UploadEnd(src, start, size, replaced || size != readStat.fileSize)
	if err != nil {
		return Result{}, fmt.Errorf("failed to scan applog %q: %w", filePath, err)
	}

	logger.Log("applogm3: reading %q from pos %d to %d", filePath, start, end)
	if end < size {
		logger.Log("applogm3: holding back %d bytes of an unfinished line in %q until the next run", size-end, filePath)
	}

	// Generate a unique destination filename to prevent conflicting file names.
	dstPath := generateUniqueLogPath(filepath.Base(filePath))
	dst, err := os.Create(dstPath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create destination file %q: %w", dstPath, err)
	}
	defer dst.Close()

	// Copy the new complete lines; a LimitReader over the file keeps Linux's in-kernel copy.
	if _, err := src.Seek(start, io.SeekStart); err != nil {
		return Result{}, fmt.Errorf("failed to seek applog %q: %w", filePath, err)
	}
	bytesCopied, err := io.Copy(dst, io.LimitReader(src, end-start))
	if err != nil {
		return Result{}, fmt.Errorf("failed to copy content from %q: %w", filePath, err)
	}

	// Close before the upload, so the app can rotate the log meanwhile.
	src.Close()

	// Update readStats for next run
	readStat.identity = fileInfo
	readStat.readPosition = start + bytesCopied
	readStat.fileSize = size
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

// appLogM3UploadEnd returns where an upload of f's [start, size) stops so it ends with a complete line.
func appLogM3UploadEnd(f io.ReaderAt, start, size int64, holdBack bool) (int64, error) {
	if !holdBack {
		return size, nil
	}

	nl := detectNewline(f)

	// A newline from here on leaves at most appLogM3MaxHeldBack bytes after it.
	from := max(start, size-appLogM3MaxHeldBack-nl.unit)
	end, err := findLastNewlineEnd(f, from, size, nl)
	if err != nil {
		return 0, err
	}
	if end > from {
		return end, nil
	}

	// No complete line: hold it all back, unless it's past the limit.
	if size-start > appLogM3MaxHeldBack {
		return size, nil
	}
	return start, nil
}

// detectNewline picks the newline encoding from the file's byte order mark.
func detectNewline(f io.ReaderAt) newline {
	var bom [2]byte
	if n, _ := f.ReadAt(bom[:], 0); n < len(bom) {
		return byteNewline
	}

	switch bom {
	case [2]byte{0xFF, 0xFE}:
		return utf16LENewline
	case [2]byte{0xFE, 0xFF}:
		return utf16BENewline
	}
	return byteNewline
}
