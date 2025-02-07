package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPing_CaptureToFile_WritesData(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err, "failed to get current working directory")
	defer os.Chdir(origDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "failed to change to temporary directory")

	// Initialize Ping with test host
	p := &Ping{
		Host: "localhost",
	}

	// Call CaptureToFile() and verify no error
	file, err := p.CaptureToFile()
	require.NoError(t, err, "CaptureToFile should not return error")
	defer file.Close()

	// Read the captured data
	data, err := os.ReadFile(pingOutputPath)
	require.NoError(t, err, "failed to read capture file")
	content := string(data)

	// Verify capture file contains expected content
	assert.Contains(t, content, "ping", "capture file should contain ping command output")
}

func TestPing_Run(t *testing.T) {
	// Setup test directory
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err, "failed to get current working directory")
	defer os.Chdir(origDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "failed to change to temporary directory")

	// Initialize Ping with test configuration
	p := &Ping{
		Host: "localhost",
	}

	// Run the capture
	result, err := p.Run()
	require.NoError(t, err, "Run() should not return error")

	// Verify output file existence and content
	outputPath := filepath.Join(tmpDir, pingOutputPath)
	_, err = os.Stat(outputPath)
	require.NoError(t, err, "output file should exist at %s", pingOutputPath)

	data, err := os.ReadFile(outputPath)
	require.NoError(t, err, "should be able to read output file")
	content := string(data)

	assert.Contains(t, content, "ping", "capture file should contain ping command output")
	assert.NotEmpty(t, result.Msg, "Run() should return non-empty result message")
}
