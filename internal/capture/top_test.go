package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTop_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "top-capture-test-*")
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
		checkContents bool
	}{
		{
			name: "successful primary command",
			setupCommands: func() {
				executils.Top = []string{"echo", "test top output"}
				executils.Top2 = nil
			},
			expectedError: false,
			checkContents: true,
		},
		{
			name: "primary fails, fallback succeeds",
			setupCommands: func() {
				executils.Top = []string{"false"} // Will exit with non-zero
				executils.Top2 = []string{"echo", "fallback output"}
			},
			expectedError: false,
			checkContents: true,
		},
		{
			name: "both commands fail",
			setupCommands: func() {
				executils.Top = []string{"false"}
				executils.Top2 = []string{"false"}
			},
			expectedError: true,
			checkContents: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up any existing output file
			os.Remove(topOutputPath)

			// Setup test commands
			tc.setupCommands()

			// Run the capture
			top := &Top{}
			file, err := top.CaptureToFile()

			// Check error condition
			if tc.expectedError {
				assert.Error(t, err)
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
			assert.Equal(t, "top.out", filepath.Base(file.Name()), "Output file should be named 'top.out'")
		})
	}
}

func TestTopH_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "toph-capture-test-*")
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
		n             int
		expectedError bool
		checkContents bool
	}{
		{
			name: "successful primary command",
			setupCommands: func() {
				executils.TopH = []string{"echo", "test top -H output"}
				executils.TopH2 = nil
			},
			n:             1,
			expectedError: false,
			checkContents: true,
		},
		{
			name: "primary fails, fallback succeeds",
			setupCommands: func() {
				executils.TopH = []string{"false"} // Will exit with non-zero
				executils.TopH2 = []string{"echo", "fallback output"}
			},
			n:             2,
			expectedError: false,
			checkContents: true,
		},
		{
			name: "both commands fail",
			setupCommands: func() {
				executils.TopH = []string{"false"}
				executils.TopH2 = []string{"false"}
			},
			n:             3,
			expectedError: true,
			checkContents: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Setup test commands
			tc.setupCommands()

			// Create TopH instance with test PID and N
			topH := &TopH{
				N: tc.n,
			}

			// Run the capture
			file, err := topH.CaptureToFile()

			// Check error condition
			if tc.expectedError {
				assert.Error(t, err)
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

			expectedFileName := fmt.Sprintf("topdashH.%d.out", topH.N)
			assert.Equal(t,
				filepath.Base(file.Name()),
				expectedFileName,
				"Output file should be named '%s'",
				expectedFileName,
			)
		})
	}
}
