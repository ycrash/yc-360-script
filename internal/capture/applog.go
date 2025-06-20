package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"yc-agent/internal/config"

	"github.com/mattn/go-zglob"
)

var compressedFileExtensions = []string{
	"zip",
	"gz",
}

// AppLog handles the capture and processing of application log files.
// It supports both compressed and uncompressed log files and can limit
// the number of lines processed from each file.
type AppLog struct {
	Capture
	Paths     config.AppLogs
	LineLimit int
}

// Run executes the log capture process for all configured paths.
// It processes each path as a glob pattern, capturing logs from all matching files.
// Returns a summary Result of all operations and any errors encountered.
func (al *AppLog) Run() (Result, error) {
	var results []Result
	var errs []error

	// Process each path as a glob pattern to handle wildcard matching
	for _, path := range al.Paths {
		matches, err := zglob.Glob(string(path))
		if err != nil {
			// Don't fail completely if one pattern is invalid - record error and continue
			results = append(results, Result{
				Msg: fmt.Sprintf("invalid glob pattern: %s", path),
				Ok:  false,
			})
			errs = append(errs, err)
			continue
		}

		// Process each file that matches the glob pattern
		for _, match := range matches {
			r, err := al.CaptureSingleAppLog(match)
			results = append(results, r)
			errs = append(errs, err)
		}
	}

	return summarizeResults(results, errs)
}

// CaptureSingleAppLog processes a single log file at the given filepath.
// It handles both compressed and uncompressed files, copies the content
// to a unique destination file, and posts the data to a configured endpoint.
// Returns a Result indicating success/failure and any error encountered.
func (al *AppLog) CaptureSingleAppLog(filePath string) (Result, error) {
	src, err := os.Open(filePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to open applog %q: %w", filePath, err)
	}
	defer src.Close()

	// Extract file information needed for processing
	fileBaseName := filepath.Base(filePath)
	fileExt := filepath.Ext(filePath)          // .zip, .log
	fileExt = strings.TrimPrefix(fileExt, ".") // zip, log
	isCompressed := isCompressedFileExt(fileExt)

	// Create a new file with a unique name to store the processed log content
	// Example: 1.appLogs.abc.log
	dstPath := generateUniqueLogPath(fileBaseName)
	dst, err := os.Create(dstPath)

	if err != nil {
		return Result{}, fmt.Errorf("applog failed to create destination file %q: %w", dstPath, err)
	}
	defer dst.Close()

	// Copy content with special handling for compressed files
	if err := al.copyLogContent(src, dst, isCompressed); err != nil {
		return Result{}, fmt.Errorf("applog failed to copy log content: %w", err)
	}

	if err := dst.Sync(); err != nil {
		return Result{}, fmt.Errorf("applog failed to sync destination file: %w", err)
	}

	// Send the log data to the configured endpoint
	data := buildPostData(fileBaseName, fileExt, isCompressed)
	msg, ok := PostData(al.Endpoint(), data, dst)

	return Result{Msg: msg, Ok: ok}, nil
}

// generateUniqueLogPath creates a unique file path for storing the log content.
// It appends a sequential number to the base filename until it finds an unused path.
// Returns the generated unique path as a string.
func generateUniqueLogPath(baseFileName string) string {
	counter := 1
	for {
		// Generate a unique filename by appending the sequential number
		// Example: 1.appLogs.abc.log
		path := fmt.Sprintf("%d.appLogs.%s", counter, baseFileName)
		if !fileExists(path) {
			return path
		}

		// Keep trying until we find an available filename
		counter++
	}
}

// copyLogContent copies the content from the source file to the destination file.
// For uncompressed files, it positions the reader at the last N lines (specified by LineLimit).
// For compressed files, it copies the entire content.
// Returns an error if any operation fails.
func (al *AppLog) copyLogContent(src, dst *os.File, isCompressed bool) error {
	if !isCompressed && al.LineLimit != -1 {
		// For uncompressed files, we only want the last N lines to avoid
		// processing extremely large log files
		if err := PositionLastLines(src, uint(al.LineLimit)); err != nil {
			return fmt.Errorf("position last lines: %w", err)
		}
	}

	// For compressed files or when LineLimit is -1, we copy everything since we can't
	// easily position partway through

	if _, err := io.Copy(dst, src); err != nil {
		return fmt.Errorf("copy file content: %w", err)
	}

	return nil
}

// isCompressedFileExt checks if the given file extension indicates
// a compressed file format (zip or gz).
// Returns true if the extension matches a known compressed format.
func isCompressedFileExt(s string) bool {
	for _, ext := range compressedFileExtensions {
		if ext == s {
			return true
		}
	}

	return false
}

func buildPostData(fileName, ext string, isCompressed bool) string {
	dt := "applog&logName=" + fileName
	if isCompressed {
		dt += "&content-encoding=" + ext
	}
	return dt
}

// summarizeResults combines multiple Results and errors into a single Result.
// It formats all messages and errors into a single string and determines overall success.
// Returns a combined Result and the last error encountered if no operation succeeded.
func summarizeResults(results []Result, errs []error) (Result, error) {
	var buf strings.Builder
	hasSuccess := false // Track if any operation succeeded

	var lastErr error
	for i, r := range results {
		fmt.Fprintf(&buf, "Msg: %s\nOk: %t", r.Msg, r.Ok)

		if r.Ok {
			hasSuccess = true
		}

		if errs[i] != nil {
			fmt.Fprintf(&buf, "\nErr: %s", errs[i].Error())
			lastErr = errs[i] // Keep track of last error for return value
		}

		buf.WriteString("\n----\n")
	}

	// Only return error if nothing succeeded - partial success is still success
	if !hasSuccess && lastErr != nil {
		return Result{
			Msg: buf.String(),
			Ok:  false,
		}, lastErr
	}

	return Result{
		Msg: buf.String(),
		Ok:  hasSuccess,
	}, nil
}
