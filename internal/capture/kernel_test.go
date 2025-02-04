package capture

import (
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKernel_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "kernel-capture-test-*")
	require.NoError(t, err, "Failed to create temp directory")
	defer os.RemoveAll(tmpDir)

	// Change to temp directory for test execution
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	defer os.Chdir(originalDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

	tests := []struct {
		name          string
		setupCommand  func()
		expectedError bool
		expectedFile  bool
		checkContents bool
	}{
		{
			name: "successful command",
			setupCommand: func() {
				executils.KernelParam = []string{"echo", "test kernel params"}
			},
			expectedError: false,
			expectedFile:  true,
			checkContents: true,
		},
		{
			name: "command fails",
			setupCommand: func() {
				executils.KernelParam = []string{"false"}
			},
			expectedError: true,
			expectedFile:  false,
			checkContents: false,
		},
		{
			name: "no command available",
			setupCommand: func() {
				executils.KernelParam = nil
			},
			expectedError: false,
			expectedFile:  true,
			checkContents: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up any existing output file
			os.Remove(kernelOutputPath)

			// Setup test command
			tc.setupCommand()

			// Run the capture
			k := &Kernel{}
			file, err := k.CaptureToFile()

			// Check error condition
			if tc.expectedError {
				assert.Error(t, err)
				assert.Nil(t, file)
				return
			}

			// Check success condition
			if !tc.expectedFile {
				assert.NoError(t, err)
				assert.Nil(t, file)
				return
			}

			// Verify successful capture
			require.NoError(t, err)
			require.NotNil(t, file)
			defer file.Close()

			// Verify the output file exists and has content
			fileInfo, err := file.Stat()
			require.NoError(t, err, "Failed to get file info")

			if tc.checkContents {
				assert.Greater(t, fileInfo.Size(), int64(0), "Captured file should not be empty")
			}
			assert.Equal(t, "kernel.out", filepath.Base(file.Name()), "Output file should be named 'kernel.out'")
		})
	}
}
