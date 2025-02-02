package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDisk_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "disk-capture-test-*")
	require.NoError(t, err, "Failed to create temp directory")
	defer os.RemoveAll(tmpDir)

	// Change to temp directory for test execution
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	defer os.Chdir(originalDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

	// Run the capture
	d := &Disk{}
	file, err := d.CaptureToFile()
	require.NoError(t, err, "CaptureToFile should not return error")
	defer file.Close()

	// Verify the output file exists and has content
	fileInfo, err := file.Stat()
	require.NoError(t, err, "Failed to get file info")

	assert.Greater(t, fileInfo.Size(), int64(0), "Captured file should not be empty")
	assert.Equal(t, "disk.out", filepath.Base(file.Name()), "Output file should be named 'disk.out'")
}
