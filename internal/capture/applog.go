package capture

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

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
// Supports files, directories, and glob patterns.
// Returns a summary Result of all operations and any errors encountered.
func (al *AppLog) Run() (Result, error) {
	pathStrings := make([]string, len(al.Paths))
	for i, path := range al.Paths {
		pathStrings[i] = string(path)

		logger.Debug().Msgf("AppLog: glob path: %s", string(path))
	}

	// Expand all paths (files, directories, globs) to individual files
	expandedPaths, expansionErr := expandPaths(pathStrings)

	var results []Result
	var errs []error

	if expansionErr != nil {
		logger.Warn().Err(expansionErr).Msg("AppLog: path expansion error")
	}

	// Process each expanded file path
	for _, filePath := range expandedPaths {
		logger.Debug().Msgf("AppLog: expanded path: %s", filePath)

		r, err := al.CaptureSingleAppLog(filePath)
		results = append(results, r)
		errs = append(errs, err)
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

// expandPaths takes a slice of paths and expands directories to individual files
// while preserving existing file and glob pattern functionality
func expandPaths(paths []string) ([]string, error) {
	var expandedPaths []string
	var errors []error

	for _, path := range paths {
		matches, err := zglob.Glob(path)
		if err != nil {
			errors = append(errors, fmt.Errorf("invalid glob pattern %s: %w", path, err))
			continue
		}

		// For each glob match, check if it's a directory
		for _, match := range matches {
			fileInfo, err := os.Stat(match)
			if err != nil {
				errors = append(errors, fmt.Errorf("cannot access %s: %w", match, err))
				continue
			}

			if fileInfo.IsDir() {
				dirFiles, err := expandDirectory(match)
				if err != nil {
					errors = append(errors, err)
					continue
				}
				expandedPaths = append(expandedPaths, dirFiles...)
			} else {
				expandedPaths = append(expandedPaths, match)
			}
		}
	}

	return expandedPaths, combineErrors(errors)
}

// expandDirectory reads a directory and returns all regular files within it
func expandDirectory(dirPath string) ([]string, error) {
	var files []string

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read directory %s: %w", dirPath, err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(dirPath, entry.Name())
		files = append(files, fullPath)
	}

	return files, nil
}

// combineErrors combines multiple errors into a single error
func combineErrors(errs []error) error {
	if len(errs) == 0 {
		return nil
	}

	var errorMessages []string
	for _, err := range errs {
		if err != nil {
			errorMessages = append(errorMessages, err.Error())
		}
	}

	if len(errorMessages) == 0 {
		return nil
	}

	return errors.New(strings.Join(errorMessages, "; "))
}
