package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAppLogsAutoDiscovery_GetOpenedFilesByProcess(t *testing.T) {
	// Helper function to check if a file is in the list of open files
	fileExistsInList := func(filename string, openFiles []string) bool {
		for _, fp := range openFiles {
			if strings.Contains(fp, filepath.Base(filename)) {
				return true
			}
		}
		return false
	}

	// Helper function to create and cleanup a temp file
	createTempFile := func(t *testing.T) *os.File {
		f, err := os.CreateTemp("", "testfile_")
		require.NoError(t, err, "Failed to create temporary file")
		t.Cleanup(func() {
			os.Remove(f.Name())
		})
		return f
	}

	t.Run("should find open file descriptor for current process", func(t *testing.T) {
		// Arrange
		f := createTempFile(t)
		defer f.Close()

		// Act
		openFiles, err := GetOpenedFilesByProcess(os.Getpid())
		require.NoError(t, err, "Expected to get list of opened files")

		// Assert
		if !fileExistsInList(f.Name(), openFiles) {
			t.Errorf("Expected to find temp file %q in open file descriptors\nGot: %v", f.Name(), openFiles)
		}
	})

	t.Run("should not find closed file descriptor", func(t *testing.T) {
		// Arrange
		f := createTempFile(t)
		f.Close() // Close immediately

		// Act
		openFiles, err := GetOpenedFilesByProcess(os.Getpid())
		require.NoError(t, err, "Expected to get list of opened files")

		// Assert
		if fileExistsInList(f.Name(), openFiles) {
			t.Errorf("Expected closed file %q to not be in open file descriptors\nGot: %v", f.Name(), openFiles)
		}
	})

	t.Run("should return error for non-existent process ID", func(t *testing.T) {
		// Arrange
		nonExistentPID := 99999999

		// Act
		openFiles, err := GetOpenedFilesByProcess(nonExistentPID)

		// Assert
		require.Error(t, err, "Expected error when checking non-existent PID %d", nonExistentPID)
		require.Empty(t, openFiles, "Expected no files for non-existent PID")
	})
}
