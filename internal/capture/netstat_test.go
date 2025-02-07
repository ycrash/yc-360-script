package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNetStat_CaptureToFile_WritesData(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir := t.TempDir()
	capturePath := filepath.Join(tmpDir, "netstat_capture.out")
	file, err := os.Create(capturePath)
	require.NoError(t, err, "failed to create temporary file")

	ns := &NetStat{
		file: file,
	}

	// Call CaptureToFile() and verify no error
	err = ns.CaptureToFile()
	require.NoError(t, err, "CaptureToFile should not return error")

	// Close the file so we can read its contents
	file.Close()

	data, err := os.ReadFile(capturePath)
	require.NoError(t, err, "failed to read capture file")
	content := string(data)

	assert.NotEmpty(t, strings.TrimSpace(content), "capture file should contain header and netstat output")
	assert.Contains(t, content, "\n", "capture file should contain newline separator")
	assert.Contains(t, content, "Proto", "capture file should contain netstat output")
}

func TestNetStat_Run(t *testing.T) {
	// Setup test directory
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err, "failed to get current working directory")
	defer func() {
		err := os.Chdir(origDir)
		assert.NoError(t, err, "failed to restore original directory")
	}()

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "failed to change to temporary directory")

	// Initialize NetStat with dummy implementation
	ns := &NetStat{sleepBetweenCaptures: 1 * time.Millisecond}

	// Run the capture
	result, err := ns.Run()
	require.NoError(t, err, "Run() should not return error")

	// Verify output file existence and content
	outputPath := filepath.Join(tmpDir, netStatOutputPath)
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "output file should exist at %s", netStatOutputPath)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err, "should be able to read output file")
	content := string(data)

	// Verify captures are properly separated
	assert.Contains(t, content, "\n\n", "output should contain separator between captures")
	assert.NotEmpty(t, result.Msg, "Run() should return non-empty result message")
}
