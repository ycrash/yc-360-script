package capture

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVMStat_CaptureToFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Skipping test for non Linux env")
	}

	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "vmstat-capture-test-*")
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
		setupCommands func()
		expectedError bool
		expectedFile  bool
		checkContents bool
	}{
		{
			name: "successful primary command",
			setupCommands: func() {
				executils.VMState = []string{"echo", "test vmstat output"}
			},
			expectedError: false,
			expectedFile:  true,
			checkContents: true,
		},
		{
			name: "command fails with non-zero exit",
			setupCommands: func() {
				executils.VMState = []string{"false"}
			},
			expectedError: false,
			expectedFile:  true,
			checkContents: true,
		},
		{
			name: "file creation error",
			setupCommands: func() {
				// Create a directory with the same name as output file to cause creation error
				os.Mkdir(vmstatOutputPath, 0755)
			},
			expectedError: true,
			expectedFile:  false,
			checkContents: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up any existing output file or directory
			os.RemoveAll(vmstatOutputPath)

			// Setup test commands
			tc.setupCommands()

			// Run the capture
			v := &VMStat{}
			file, err := v.CaptureToFile()

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
			assert.Equal(t, "vmstat.out", filepath.Base(file.Name()), "Output file should be named 'vmstat.out'")
		})
	}
}
