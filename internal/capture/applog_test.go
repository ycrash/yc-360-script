package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	// Captured copies are written to the working directory.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create initial file to force unique name generation
	existingFile := "1.appLogs.test.log"
	err := os.WriteFile(existingFile, []byte("dummy"), 0644)
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
	// Captured copies are written to the working directory.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create test log file with sample content
	inputFileName := "test.log"
	inputContent := "line1\nline2\nline3\n"
	err := os.WriteFile(inputFileName, []byte(inputContent), 0644)
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

	// Last 2 lines should be present due to LineLimit: 2
	assert.Equal(t, "line2\nline3\n", string(outputContent), "should contain the last 2 lines")
}

// TestCaptureSingleAppLog_Compressed tests capturing a compressed log file.
func TestCaptureSingleAppLog_Compressed(t *testing.T) {
	// Captured copies are written to the working directory.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)

	// Create a sample compressed test file
	inputFileName := "test.gz"
	inputContent := "compressed data here"
	err := os.WriteFile(inputFileName, []byte(inputContent), 0644)
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

func TestExpandPaths(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.log"), []byte("a"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.log"), []byte("b"), 0644))

	got, err := expandPaths([]string{
		filepath.Join(dir, "*.log"),
		filepath.Join(dir, "a.log"),
		dir,              // directory expands to a.log and b.log
		dir + "/./b.log", // same file, different spelling
	})

	require.NoError(t, err)
	assert.ElementsMatch(t, []string{filepath.Join(dir, "a.log"), filepath.Join(dir, "b.log")}, got) // zglob returns matches in no fixed order
}

func TestExpandPaths_FileIdentity(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "two", "sub"), 0755))
	require.NoError(t, os.Mkdir(filepath.Join(root, "one"), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "two", "server.log"), []byte("TWO"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "one", "server.log"), []byte("ONE"), 0644))
	if err := os.Symlink(filepath.Join(root, "two", "sub"), filepath.Join(root, "one", "link")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("cannot create symlink: %v", err)
		}
		require.NoError(t, err)
	}

	// Reaches two/server.log via the symlink; filepath.Clean/Join would yield one/server.log.
	viaLink := filepath.Join(root, "one", "link") + "/../server.log"

	t.Run("keeps the configured spelling when .. follows a symlink", func(t *testing.T) {
		got, err := expandPaths([]string{viaLink})
		require.NoError(t, err)
		require.Equal(t, []string{viaLink}, got)

		content, err := os.ReadFile(got[0])
		require.NoError(t, err)
		assert.Equal(t, "TWO", string(content), "must capture the file the filesystem resolves, not the lexically cleaned one")
	})

	t.Run("folds spellings through a symlink", func(t *testing.T) {
		got, err := expandPaths([]string{viaLink, filepath.Join(root, "two", "server.log")})
		require.NoError(t, err)
		assert.Equal(t, []string{viaLink}, got, "same file, first spelling kept")
	})

	t.Run("folds relative and absolute spellings", func(t *testing.T) {
		t.Chdir(root)
		relative := filepath.Join("two", "server.log")
		got, err := expandPaths([]string{relative, filepath.Join(root, "two", "server.log")})
		require.NoError(t, err)
		assert.Equal(t, []string{relative}, got, "same file, first spelling kept")
	})

	// A relative spelling with ".." after a symlink must fold with the absolute one.
	t.Run("folds a relative spelling with .. after a symlink", func(t *testing.T) {
		t.Chdir(root)
		relativeViaLink := filepath.Join("one", "link") + "/../server.log"
		got, err := expandPaths([]string{relativeViaLink, filepath.Join(root, "two", "server.log")})
		require.NoError(t, err)
		assert.Equal(t, []string{relativeViaLink}, got, "same file, first spelling kept")
	})
}

func TestRun(t *testing.T) {
	t.Run("should process multiple log files matching glob pattern", func(t *testing.T) {
		// Captured copies are written to the working directory.
		tmpDir := t.TempDir()
		t.Chdir(tmpDir)

		// Create test log files
		err := os.WriteFile("log1.log", []byte("content1"), 0644)
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

	t.Run("should capture a file matched by overlapping paths once", func(t *testing.T) {
		// Captured copies are written to the working directory.
		tmpDir := t.TempDir()
		t.Chdir(tmpDir)

		logDir := filepath.Join(tmpDir, "logs")
		require.NoError(t, os.Mkdir(logDir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(logDir, "server.log"), []byte("content"), 0644))

		appLog := &AppLog{
			Paths:     config.AppLogs{config.AppLog(filepath.Join(logDir, "*.log")), config.AppLog(filepath.Join(logDir, "server.log"))},
			LineLimit: 3000,
		}

		// Run
		_, err := appLog.Run()

		// Verify
		assert.NoError(t, err)
		assert.FileExists(t, "1.appLogs.server.log")
		assert.NoFileExists(t, "2.appLogs.server.log", "server.log should be captured once")
	})

	t.Run("should handle invalid glob pattern", func(t *testing.T) {
		appLog := &AppLog{
			Paths: config.AppLogs{"["}, // invalid glob pattern
		}

		result, _ := appLog.Run()

		// Invalid glob pattern results in no files processed, so result.Ok is false.
		// The implementation logs a warning but doesn't return an error.
		assert.False(t, result.Ok, "result should indicate failure for invalid glob pattern")
	})
}
