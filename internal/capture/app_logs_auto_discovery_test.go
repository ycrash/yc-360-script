package capture

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsMostlyASCII verifies that the IsMostlyASCII function correctly identifies
// text content as ASCII or non-ASCII based on a 70% threshold. The function should
// handle various scenarios including pure ASCII, mixed content, and edge cases.
func TestIsMostlyASCII(t *testing.T) {
	// Tests 100% ASCII content
	t.Run("AllASCII", func(t *testing.T) {
		// Setup: String with only ASCII characters
		data := []byte("Hello, World!")

		// Test: Check if content is mostly ASCII
		result := IsMostlyASCII(data)

		// Expect: Should return true as all characters are ASCII
		assert.True(t, result, "should identify pure ASCII content correctly")
	})

	// Tests content with equal ASCII and non-ASCII distribution
	t.Run("50PercentASCII", func(t *testing.T) {
		// Setup: 5 ASCII characters and 5 non-ASCII characters (Cyrillic)
		// ASCII: ABCDE (5 chars)
		// Non-ASCII: абвгд (5 chars)
		data := []byte("ABCDEабвгд")

		// Test: Check if content meets ASCII threshold
		result := IsMostlyASCII(data)

		// Expect: Should return false as 50% is below the 70% threshold
		assert.False(t, result, "should reject content with only 50% ASCII")
	})

	// Tests content exactly at the threshold boundary
	t.Run("Threshold70Percent", func(t *testing.T) {
		// Setup: 7 ASCII chars and 3 non-ASCII chars = 70% ASCII
		// ASCII: ABCDEFg (7 chars)
		// Non-ASCII: абв (3 chars)
		data := []byte("ABCDEFgабв")

		// Test: Check if content at threshold is handled correctly
		result := IsMostlyASCII(data)

		// Expect: Should return false as we need >70% (not >=70%)
		assert.False(t, result, "should reject content exactly at 70% threshold")
	})

	// Tests empty input handling
	t.Run("EmptySlice", func(t *testing.T) {
		// Setup: Empty byte slice
		data := []byte{}

		// Test and Expect: Function should not panic
		assert.NotPanics(t, func() {
			IsMostlyASCII(data)
		}, "should handle empty input without panicking")
	})
}

// TestMatchLogPattern verifies that log file patterns are correctly matched based on filename.
// It tests various filename scenarios.
func TestMatchLogPattern(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"example.log", true},           // standard .log file
		{"example-rotated.log.1", true}, // rotated log file matches second pattern (.*log.*\..*)
		{"mylog.txt", true},             // file containing 'log' with extension matches second pattern
		{"logfile", false},              // no match: missing extension (no dot after 'log')
		{"output.LOG", false},           // no match: case-sensitive patterns
		{"test.txt", false},             // no match: not a log file
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := matchLogPattern(tt.filename)
			assert.Equal(t, tt.expected, got,
				"matchLogPattern(%q): got %v, want %v",
				tt.filename, got, tt.expected)
		})
	}
}

// TestGetLastNBytes verifies the behavior of reading the last N bytes from files
// under different scenarios: files smaller than requested size, exact size matches,
// partial reads, and error conditions.
func TestGetLastNBytes(t *testing.T) {
	// Tests reading from a file smaller than requested bytes
	t.Run("FileSmallerThanN", func(t *testing.T) {
		// Setup: Create temp file with content smaller than requested bytes
		tmpFile, err := os.CreateTemp("", "smallFile*.txt")
		require.NoError(t, err, "failed to create temp file")
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()

		content := []byte("Hello")
		_, err = tmpFile.Write(content)
		require.NoError(t, err, "failed to write to temp file")

		// Test: Request more bytes than file contains
		got, err := getLastNBytes(tmpFile.Name(), 10)
		require.NoError(t, err)

		// Expect: Get entire file content
		assert.Equal(t, "Hello", string(got))
	})

	// Tests reading exactly the size of the file
	t.Run("ExactSize", func(t *testing.T) {
		// Setup: Create temp file with content matching requested size
		tmpFile, err := os.CreateTemp("", "exactFile*.txt")
		require.NoError(t, err, "failed to create temp file")
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()

		content := []byte("1234567890")
		_, err = tmpFile.Write(content)
		require.NoError(t, err, "failed to write content")

		// Test: Request exactly the file size
		got, err := getLastNBytes(tmpFile.Name(), int64(len(content)))
		require.NoError(t, err)

		// Expect: Get entire file content
		assert.Equal(t, string(content), string(got))
	})

	// Tests reading the tail portion of a file
	t.Run("TailBytes", func(t *testing.T) {
		// Setup: Create temp file with content longer than requested
		tmpFile, err := os.CreateTemp("", "tailFile*.txt")
		require.NoError(t, err, "failed to create temp file")
		defer os.Remove(tmpFile.Name())
		defer tmpFile.Close()

		content := []byte("Hello, World!")
		_, err = tmpFile.Write(content)
		require.NoError(t, err, "failed to write content")

		// Test: Request last 6 bytes of content
		got, err := getLastNBytes(tmpFile.Name(), 6)
		require.NoError(t, err, "getLastNBytes() failed")

		// Expect: Get only the last 6 bytes ("World!")
		assert.Equal(t, "World!", string(got), "unexpected tail content")
	})

	// Tests handling of non-existent files
	t.Run("NonExistentFile", func(t *testing.T) {
		// Test: Attempt to read from non-existent file
		_, err := getLastNBytes("no_such_file.txt", 10)

		// Expect: Error should be returned
		assert.Error(t, err, "should error on non-existent file")
	})
}

func TestDiscoverOpenedLogFilesByProcess(t *testing.T) {
	// Skip test on non-Linux platforms since the functionality is Linux-specific
	if runtime.GOOS != "linux" {
		return
	}

	// Test setup
	pid := os.Getpid()
	dir, err := os.MkdirTemp("", "discoverLogsTest")
	require.NoError(t, err, "failed to create temp directory")
	defer os.RemoveAll(dir)

	// Test cases define different types of files we want to verify
	// Each case tests a specific aspect of log file detection
	testCases := []struct {
		name           string // Name of the test file
		content        []byte // Content to write to the file
		shouldBeASCII  bool   // Whether content meets ASCII threshold
		shouldMatchLog bool   // Whether filename matches log patterns
	}{
		{
			name:           "test1.log",
			content:        []byte("This is a mostly ASCII log file."),
			shouldBeASCII:  true,
			shouldMatchLog: true,
		},
		{
			name:           "test2.log",
			content:        []byte("абвгд Пример не-ASCII"),
			shouldBeASCII:  false,
			shouldMatchLog: true,
		},
		{
			name:           "test3.txt",
			content:        []byte("Some ASCII content but not a .log"),
			shouldBeASCII:  true,
			shouldMatchLog: false,
		},
		{
			name:           "testlog.out",
			content:        []byte("Another ASCII log-like file."),
			shouldBeASCII:  true,
			shouldMatchLog: true,
		},
	}

	// Create and open all test files
	var openedFiles []*os.File
	defer func() {
		// Cleanup: close all opened files
		for _, f := range openedFiles {
			if f != nil {
				f.Close()
			}
		}
	}()

	// Set up test files and keep them open
	for _, tc := range testCases {
		fullPath := filepath.Join(dir, tc.name)

		// Create and write content
		err := os.WriteFile(fullPath, tc.content, 0644)
		require.NoError(t, err, "failed to create and write to file %q", tc.name)

		// Keep file open for detection
		f, err := os.Open(fullPath)
		require.NoError(t, err, "failed to open file %q", tc.name)
		openedFiles = append(openedFiles, f)
	}

	// Run the discovery function
	discoveredFiles, err := DiscoverOpenedLogFilesByProcess(pid)
	require.NoError(t, err, "DiscoverOpenedLogFilesByProcess failed")

	// Convert results to a map for easier verification
	discoveredSet := make(map[string]bool)
	for _, path := range discoveredFiles {
		discoveredSet[path] = true
	}

	// Verify each test case
	for _, tc := range testCases {
		fullPath := filepath.Join(dir, tc.name)

		// A file should be discovered only if it matches both conditions:
		// 1. Filename matches log pattern
		// 2. Content is mostly ASCII
		expectedDiscovery := tc.shouldMatchLog && tc.shouldBeASCII
		actuallyDiscovered := discoveredSet[fullPath]

		assert.Equal(t, expectedDiscovery, actuallyDiscovered,
			"file %q: unexpected discovery status (expected=%v, actual=%v)",
			tc.name, expectedDiscovery, actuallyDiscovered)
	}
}
