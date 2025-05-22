package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

// DefaultScriptTimeout is the default timeout for script execution
const DefaultScriptTimeout = 5 * time.Minute

// ExtendedData handles the execution of a custom script and collection of its output artifacts.
type ExtendedData struct {
	Capture
	Script     string        // Path to the custom script
	DataFolder string        // Path to the folder where script artifacts are stored
	Timeout    time.Duration // Timeout for script execution, defaults to DefaultScriptTimeout
}

// Run executes the custom script and uploads all files from the data folder
// to the specified endpoint.
func (ed *ExtendedData) Run() (Result, error) {
	// Ensure the data folder exists
	if err := os.MkdirAll(ed.DataFolder, 0755); err != nil {
		errMsg := fmt.Sprintf("ExtendedData: failed to create data folder %s: %v", ed.DataFolder, err)
		logger.Log(errMsg)
		return Result{Msg: errMsg, Ok: false}, err
	}

	// Clear existing files in the data folder
	if err := ed.clearDataFolder(); err != nil {
		logger.Log("ExtendedData: failed to clear data folder: %v", err)
	}

	// Execute the custom script with timeout
	if err := ed.executeScript(); err != nil {
		// We log the error but continue to upload any files that might have been generated
		logger.Log("ExtendedData: error while executing custom script: %v", err)
	}

	// Copy files from data folder to current directory with "ed-" prefix
	err := ed.captureEdFiles()
	if err != nil {
		errMsg := fmt.Sprintf("ExtendedData: failed to capture files: %v", err)
		logger.Log(errMsg)
		return Result{Msg: errMsg, Ok: false}, err
	}

	// Upload the captured files
	return ed.uploadCapturedFiles()
}

// clearDataFolder removes all files from the data folder
func (ed *ExtendedData) clearDataFolder() error {
	entries, err := os.ReadDir(ed.DataFolder)
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to read data folder: %w", err)
	}

	for _, entry := range entries {
		path := filepath.Join(ed.DataFolder, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("ExtendedData: failed to remove %s: %w", path, err)
		}
	}

	return nil
}

// executeScript runs the custom script with a timeout
func (ed *ExtendedData) executeScript() error {
	logger.Log("ExtendedData: executing custom script: %s", ed.Script)

	// Create a temporary file for script output
	logFile, err := os.Create(filepath.Join(ed.DataFolder, "script_execution.log"))
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to create script log file: %w", err)
	}
	defer logFile.Close()

	// Start the script
	cmd, err := executils.CommandStartInBackgroundToWriter(logFile, []string{ed.Script})
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to start custom script: %w", err)
	}
	ed.Cmd = cmd

	if cmd.IsSkipped() {
		logger.Log("ExtendedData: custom script execution was skipped")
		return nil
	}

	// Set default timeout if not specified
	if ed.Timeout == 0 {
		ed.Timeout = DefaultScriptTimeout
	}

	// Set up a timeout channel
	timeout := time.After(ed.Timeout)
	cmdWaitCh := make(chan error, 1)

	// Wait for the command in a goroutine
	go func() {
		cmdWaitCh <- cmd.Wait()
	}()

	// Wait for either completion or timeout
	select {
	case err := <-cmdWaitCh:
		if err != nil {
			return fmt.Errorf("ExtendedData: custom script failed: %w", err)
		}
		if cmd.ExitCode() != 0 {
			return fmt.Errorf("ExtendedData: custom script exited with non-zero code: %d", cmd.ExitCode())
		}
		logger.Log("ExtendedData: custom script completed successfully")
	case <-timeout:
		logger.Log("ExtendedData: custom script timed out after %v, terminating", ed.Timeout)
		if err := cmd.Kill(); err != nil {
			logger.Log("ExtendedData: failed to kill timed out script: %v", err)
		}
		return fmt.Errorf("ExtendedData: custom script execution timed out after %v", ed.Timeout)
	}

	return nil
}

// captureEdFiles copies files from the data folder to the current directory with "ed-" prefix
func (ed *ExtendedData) captureEdFiles() error {
	entries, err := os.ReadDir(ed.DataFolder)
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to read data folder: %w", err)
	}

	if len(entries) == 0 {
		logger.Log("ExtendedData: no files found in data folder %s", ed.DataFolder)
		return nil
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip directories
		}

		fileName := entry.Name()
		filePath := filepath.Join(ed.DataFolder, fileName)

		// Create a new filename with "ed-" prefix
		newFileName := "ed-" + fileName

		// Copy the file to current directory
		err := ed.copyFile(filePath, newFileName)
		if err != nil {
			logger.Log("ExtendedData: failed to copy file %s to %s: %v", filePath, newFileName, err)
		}
	}

	return nil
}

// copyFile copies a file from source to destination
func (ed *ExtendedData) copyFile(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to open source file: %w", err)
	}
	defer srcFile.Close()

	dstFile, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to create destination file: %w", err)
	}
	defer dstFile.Close()

	_, err = io.Copy(dstFile, srcFile)
	if err != nil {
		return fmt.Errorf("ExtendedData: failed to copy content: %w", err)
	}

	return nil
}

// uploadCapturedFiles uploads files with "ed-" prefix from the current directory
func (ed *ExtendedData) uploadCapturedFiles() (Result, error) {
	// Get current directory entries
	entries, err := os.ReadDir(".")
	if err != nil {
		return Result{
			Msg: fmt.Sprintf("ExtendedData: failed to read current directory: %v", err),
			Ok:  false,
		}, err
	}

	successCount := 0
	failCount := 0
	var lastError error

	// Filter files with "ed-" prefix
	for _, entry := range entries {
		if entry.IsDir() {
			continue // Skip directories
		}

		fileName := entry.Name()

		// Only process files with "ed-" prefix
		if !strings.HasPrefix(fileName, "ed-") {
			continue
		}

		file, err := os.Open(fileName)
		if err != nil {
			logger.Log("ExtendedData: failed to open file %s: %v", fileName, err)
			failCount++
			lastError = err
			continue
		}

		fileExt := filepath.Ext(fileName)
		fileExt = strings.TrimPrefix(fileExt, ".") // Remove leading dot from extension
		fileBaseName := strings.TrimSuffix(fileName, filepath.Ext(fileName))
		isCompressed := isCompressedFileExt(fileExt)

		// Build custom post data for extended data
		data := "ed&fileName=" + fileBaseName
		if isCompressed {
			data += "&content-encoding=" + fileExt
		}

		msg, ok := PostData(ed.Endpoint(), data, file)
		file.Close()

		if ok {
			successCount++
		} else {
			failCount++
			lastError = fmt.Errorf("upload failed: %s", msg)
		}
	}

	if failCount > 0 {
		return Result{
			Msg: fmt.Sprintf("uploaded %d files, %d failed", successCount, failCount),
			Ok:  successCount > 0, // Consider partial success if at least one file was uploaded
		}, lastError
	}

	if successCount == 0 {
		return Result{
			Msg: "no files with 'ed-' prefix found for upload",
			Ok:  true,
		}, nil
	}

	return Result{
		Msg: fmt.Sprintf("successfully uploaded %d files", successCount),
		Ok:  true,
	}, nil
}
