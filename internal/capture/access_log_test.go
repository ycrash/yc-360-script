package capture

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCaptureToFile tests the behavior of the CaptureToFile() method from AccessLog.
func TestAccessLog_CaptureToFile(t *testing.T) {
	t.Run("success from scratch", func(t *testing.T) {
		// Create a temp directory for test files
		tempDir := t.TempDir()

		// Create a source file with some test content
		sourcePath := filepath.Join(tempDir, "test_access.log")
		err := os.WriteFile(sourcePath, []byte("Hello, World!\n"), 0644)
		require.NoError(t, err, "failed to write source file")

		// Destination/capture file
		capturePath := filepath.Join(tempDir, "capture_output.log")

		// Initialize AccessLog with positions starting from zero
		a := &AccessLog{
			SourcePath:  sourcePath,
			CapturePath: capturePath,
			Position:    0,
		}

		// Call CaptureToFile()
		file, err := a.CaptureToFile()
		require.NoError(t, err, "expected no error during CaptureToFile")
		require.NotNil(t, file, "returned file should not be nil")
		defer file.Close()

		// Confirm file was created and has the correct content
		data, err := os.ReadFile(capturePath)
		require.NoError(t, err, "failed to read capture file")
		expected := "Hello, World!\n"
		assert.Equal(t, expected, string(data), "capture file content mismatch")

		// Check that the AccessLog position is updated
		assert.Equal(t, int64(len(expected)), a.Position, "access log position should be updated correctly")
	})

	t.Run("capture from a nonzero offset", func(t *testing.T) {
		tempDir := t.TempDir()

		// Write a longer file
		sourcePath := filepath.Join(tempDir, "offset_test_access.log")
		content := []byte("Line1\nLine2\nLine3\n")
		err := os.WriteFile(sourcePath, content, 0644)
		require.NoError(t, err, "failed to write source file")

		capturePath := filepath.Join(tempDir, "capture_output_offset.log")

		// Initialize AccessLog with an offset that starts after "Line1\n"
		offset := int64(len("Line1\n"))
		a := &AccessLog{
			SourcePath:  sourcePath,
			CapturePath: capturePath,
			Position:    offset,
		}

		// Call CaptureToFile()
		file, err := a.CaptureToFile()
		require.NoError(t, err, "expected no error during CaptureToFile")
		require.NotNil(t, file, "returned file should not be nil")
		defer file.Close()

		// Read the capture file
		data, err := os.ReadFile(capturePath)
		require.NoError(t, err, "failed to read capture file")

		// Expected content to be everything after "Line1\n"
		expected := "Line2\nLine3\n"
		assert.Equal(t, expected, string(data), "capture file content mismatch")

		// Position should be old offset + length of what we just read
		assert.Equal(t, offset+int64(len(expected)), a.Position, "Position should be updated correctly")
	})

	t.Run("error when source file does not exist", func(t *testing.T) {
		tempDir := t.TempDir()

		// Provide a non-existent source file
		sourcePath := filepath.Join(tempDir, "does_not_exist.log")
		capturePath := filepath.Join(tempDir, "dummy_capture.log")

		a := &AccessLog{
			SourcePath:  sourcePath,
			CapturePath: capturePath,
		}

		// Expect an error due to missing source file
		file, err := a.CaptureToFile()
		require.Error(t, err, "expected an error for non-existent source file")
		// If we somehow get a file handle, close it.
		if file != nil {
			file.Close()
		}
	})

	t.Run("uses default capture path if not set", func(t *testing.T) {
		tempDir := t.TempDir()

		// Write a simple source file
		sourcePath := filepath.Join(tempDir, "test_access.log")
		content := []byte("DefaultCapturePath\n")
		err := os.WriteFile(sourcePath, content, 0644)
		require.NoError(t, err, "failed to write source file")

		// Notice we are not specifying CapturePath
		a := &AccessLog{
			SourcePath: sourcePath,
			Position:   0,
		}

		file, err := a.CaptureToFile()
		require.NoError(t, err, "expected no error during CaptureToFile")
		require.NotNil(t, file, "returned file should not be nil")
		defer file.Close()

		// By default, it uses "accesslog.out" in the current working directory
		info, err := os.Stat("accesslog.out")
		require.NoError(t, err, "expected accesslog.out to be created")
		assert.NotZero(t, info.Size(), "expected accesslog.out to have content, but size=0")

		// Clean up "accesslog.out" from the current directory
		os.Remove("accesslog.out")
	})

	t.Run("capture multiple times picks up from last position", func(t *testing.T) {
		tempDir := t.TempDir()

		// Create a source file with initial content
		sourcePath := filepath.Join(tempDir, "multi_capture.log")
		initialContent := []byte("First call content\n")
		err := os.WriteFile(sourcePath, initialContent, 0644)
		require.NoError(t, err, "failed to create source file")

		capturePath := filepath.Join(tempDir, "multi_capture_output.log")

		// Initialize AccessLog pointing to our source
		a := &AccessLog{
			SourcePath:  sourcePath,
			CapturePath: capturePath,
			Position:    0,
		}

		// First call to CaptureToFile()
		file1, err := a.CaptureToFile()
		require.NoError(t, err, "expected no error during the first CaptureToFile")
		require.NotNil(t, file1, "returned file is nil")
		file1.Close()

		// Verify first capture
		gotData1, err := os.ReadFile(capturePath)
		require.NoError(t, err, "failed to read capture file after first call")
		assert.Equal(t, initialContent, gotData1, "first capture mismatch")

		// Check updated position after first capture
		assert.Equal(t, int64(len(initialContent)), a.Position, "position should match length of initial content")

		// Now append more content to the source file
		moreContent := []byte("Second call content\n")
		appendToFile(t, sourcePath, string(moreContent))

		// Second call to CaptureToFile()
		file2, err := a.CaptureToFile()
		require.NoError(t, err, "expected no error on second call")
		require.NotNil(t, file2, "returned file is nil")
		file2.Close()

		// Verify second capture file
		gotData2, err := os.ReadFile(capturePath)
		require.NoError(t, err, "failed to read capture file after second call")

		// We expect only the newly appended content in the second capture file
		// because we recreate the capture output each time, reading only new data from the source.
		assert.Equal(t, moreContent, gotData2, "second capture mismatch")

		// Verify position was updated again
		expectedPosition := int64(len(initialContent) + len(moreContent))
		assert.Equal(t, expectedPosition, a.Position, "position should be updated after second capture")
	})
}

// appendToFile is a small helper to append text to an existing file.
func appendToFile(t *testing.T, filePath, text string) {
	t.Helper()
	f, err := os.OpenFile(filePath, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err, "failed to open file for append")
	defer f.Close()

	_, err = io.WriteString(f, text)
	require.NoError(t, err, "failed to append text")
}
