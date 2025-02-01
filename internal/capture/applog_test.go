package capture

import (
	"fmt"
	"os"
	"testing"

	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBuildPostData verifies that buildPostData produces the expected string.
func TestBuildPostData(t *testing.T) {
	t.Run("should build post data for non-compressed file", func(t *testing.T) {
		data := buildPostData("app.log", "log", false)
		assert.Equal(t, "applog&logName=app.log", data)
	})

	t.Run("should build post data for compressed file", func(t *testing.T) {
		data := buildPostData("app.gz", "gz", true)
		assert.Equal(t, "applog&logName=app.gz&content-encoding=gz", data)
	})
}

// TestGenerateUniqueLogPath verifies that generateUniqueLogPath returns a filename that does not exist.
func TestGenerateUniqueLogPath(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	originalWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(originalWd)

	err = os.Chdir(tmpDir)
	require.NoError(t, err)

	// Create initial file to force unique name generation
	existingFile := "1.appLogs.test.log"
	err = os.WriteFile(existingFile, []byte("dummy"), 0644)
	require.NoError(t, err)

	uniquePath := generateUniqueLogPath("test.log")

	// For this simple algorithm, we expect the next unique name to be "2.appLogs.test.log".
	assert.NotEqual(t, existingFile, uniquePath, "should not return existing file path")
	assert.Equal(t, "2.appLogs.test.log", uniquePath, "should generate expected unique name")
}

// TestSummarizeResults verifies that summarizeResults aggregates the result messages and errors.
func TestSummarizeResults(t *testing.T) {
	// given
	results := []Result{
		{Msg: "success", Ok: true},
		{Msg: "failure", Ok: false},
	}
	errs := []error{nil, fmt.Errorf("error message")}

	// when
	summary, err := summarizeResults(results, errs)

	// then
	assert.True(t, summary.Ok, "summary should be OK if at least one result succeeded")
	assert.NoError(t, err, "should not return error when at least one success exists")

	assert.Contains(t, summary.Msg, "success", "summary should contain success message")
	assert.Contains(t, summary.Msg, "failure", "summary should contain failure message")
	assert.Contains(t, summary.Msg, "error message", "summary should contain error message")
}

// TestCaptureSingleAppLog_NonCompressed tests capturing a non-compressed log file.
func TestCaptureSingleAppLog_NonCompressed(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	originalWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(originalWd)

	err = os.Chdir(tmpDir)
	require.NoError(t, err)

	// Create test log file with sample content
	inputFileName := "test.log"
	inputContent := "line1\nline2\nline3\n"
	err = os.WriteFile(inputFileName, []byte(inputContent), 0644)
	require.NoError(t, err, "failed to create input file")

	appLog := &AppLog{LineLimit: 2}

	// when
	_, err = appLog.CaptureSingleAppLog(inputFileName)

	// then
	assert.NoError(t, err)

	expectedOutputPath := "1.appLogs.test.log"

	// Read and verify the content of the destination file.
	outputContent, err := os.ReadFile(expectedOutputPath)
	require.NoError(t, err, "should be able to read output file")

	// Only last line should be present due to LineLimit: 2
	assert.Equal(t, "line3\n", string(outputContent), "should contain only the last line")
}

// TestCaptureSingleAppLog_Compressed tests capturing a compressed log file.
func TestCaptureSingleAppLog_Compressed(t *testing.T) {
	// Change working directory to a temporary directory.
	tmpDir := t.TempDir()
	originalWd, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(originalWd)

	err = os.Chdir(tmpDir)
	require.NoError(t, err)

	// Create a sample compressed test file
	inputFileName := "test.gz"
	inputContent := "compressed data here"
	err = os.WriteFile(inputFileName, []byte(inputContent), 0644)
	require.NoError(t, err, "failed to create compressed input file")

	// For a compressed file, the code will not call PositionLastLines.
	appLog := &AppLog{LineLimit: 1}

	// when
	_, err = appLog.CaptureSingleAppLog(inputFileName)

	// then
	assert.NoError(t, err)

	expectedOutputPath := "1.appLogs.test.gz"
	outputContent, err := os.ReadFile(expectedOutputPath)
	require.NoError(t, err, "should be able to read compressed output file")

	assert.Equal(t, inputContent, string(outputContent),
		"compressed file content should be copied without modification")
}

func TestRun(t *testing.T) {
	t.Run("should process multiple log files matching glob pattern", func(t *testing.T) {
		// Change working directory to a temporary directory.
		tmpDir := t.TempDir()
		originalWd, err := os.Getwd()
		require.NoError(t, err)
		defer os.Chdir(originalWd)

		err = os.Chdir(tmpDir)
		require.NoError(t, err)

		// Create test log files
		err = os.WriteFile("log1.log", []byte("content1"), 0644)
		require.NoError(t, err)
		err = os.WriteFile("log2.log", []byte("content2"), 0644)
		require.NoError(t, err)

		appLog := &AppLog{
			Paths:     config.AppLogs{"*.log"},
			LineLimit: 3000,
		}

		// Run
		result, err := appLog.Run()

		// Verify
		assert.NoError(t, err)
		assert.Contains(t, result.Msg, "log1.log", "result should mention first log file")
		assert.Contains(t, result.Msg, "log2.log", "result should mention second log file")
	})

	t.Run("should handle invalid glob pattern", func(t *testing.T) {
		// Setup
		appLog := &AppLog{
			Paths: config.AppLogs{"["}, // invalid glob pattern
		}

		// Run
		result, err := appLog.Run()

		// Verify
		assert.Error(t, err, "should return error for invalid glob pattern")
		assert.False(t, result.Ok, "result should indicate failure")
	})
}
