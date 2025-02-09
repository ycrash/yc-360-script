package capture

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTop4M3_CaptureToFile_WritesData(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err, "failed to get current working directory")
	defer os.Chdir(origDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "failed to change to temporary directory")

	// Initialize Top4M3
	top := &Top4M3{sleepBetweenCaptures: 1 * time.Millisecond}

	// Call CaptureToFile() and verify no error
	file, err := top.captureToFile()
	require.NoError(t, err, "captureToFile should not return error")
	defer file.Close()

	// Read the captured data
	data, err := os.ReadFile(top4m3OutputPath)
	require.NoError(t, err, "failed to read capture file")
	content := string(data)

	// Verify capture file contains expected content
	// Looking for multiple iterations since we know it runs 3 times
	assert.Contains(t, content, "\n\n\n", "capture file should contain iteration separators")
	assert.Contains(t, content, "top", "capture file should contain top command output")
}
