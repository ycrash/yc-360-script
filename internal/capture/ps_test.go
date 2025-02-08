package capture

import (
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPS_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "ps-capture-test-*")
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
				executils.PS = []string{"echo", "test ps output"}
				executils.PS2 = nil
			},
			expectedError: false,
			checkContents: true,
		},
		{
			name: "primary fails, fallback succeeds",
			setupCommands: func() {
				executils.PS = []string{"false"} // Will exit with non-zero
				executils.PS2 = []string{"echo", "fallback output"}
			},
			expectedError: false,
			checkContents: true,
		},
		{
			name: "both commands fail",
			setupCommands: func() {
				executils.PS = []string{"false"}
				executils.PS2 = []string{"false"}
			},
			expectedError: true,
			checkContents: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Clean up any existing output file
			os.Remove(psOutputPath)

			// Setup test commands
			tc.setupCommands()

			// Run the capture
			p := &PS{}
			file, err := p.CaptureToFile()

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
			assert.Equal(t, "ps.out", filepath.Base(file.Name()), "Output file should be named 'ps.out'")
		})
	}
}
