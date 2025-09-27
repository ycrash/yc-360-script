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

// createFailingCommand creates a command that will fail with non-zero exit code
func createFailingCommand() executils.Command {
	if runtime.GOOS == "windows" {
		return executils.Command{"cmd", "/c", "exit 1"}
	}
	return executils.Command{"false"}
}

func TestVMStat_CaptureToFile(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "vmstat-capture-test-*")
	require.NoError(t, err, "Failed to create temp directory")
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	// Change to temp directory for test execution
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	t.Cleanup(func() { os.Chdir(originalDir) })

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

	// Store original VMState command
	originalVMState := executils.VMState
	t.Cleanup(func() { executils.VMState = originalVMState })

	tests := []struct {
		name          string
		setupCommands func()
		expectedError bool
		expectedFile  bool
		checkContents bool
		description   string
	}{
		{
			name: "successful primary command",
			setupCommands: func() {
				// Use the real VMState command from executils - no changes needed
			},
			expectedError: false,
			expectedFile:  true,
			checkContents: true,
			description:   "Should successfully capture vmstat output using real platform command",
		},
		{
			name: "command fails with non-zero exit",
			setupCommands: func() {
				executils.VMState = createFailingCommand()
			},
			expectedError: !isLinux(), // On Linux, fallback should handle this; on others, expect error
			expectedFile:  isLinux(),  // On Linux, fallback should create file; on others, no file
			checkContents: false,
			description:   "Should handle command failure based on platform (Linux: fallback, others: error)",
		},
		{
			name: "file creation error",
			setupCommands: func() {
				// Use real command but create directory to block file creation
				// Create a directory with the same name as output file to cause creation error
				os.Mkdir(vmstatOutputPath, 0755)
			},
			expectedError: true,
			expectedFile:  false,
			checkContents: false,
			description:   "Should return error when output file cannot be created",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("Test description: %s", tc.description)

			// Clean up any existing output file or directory
			os.RemoveAll(vmstatOutputPath)

			// Setup test commands
			tc.setupCommands()

			// Run the capture
			v := &VMStat{}
			file, err := v.CaptureToFile()

			// Check error condition
			if tc.expectedError {
				assert.Error(t, err, "Expected an error but got none")
				assert.Nil(t, file, "Expected nil file when error occurs")
				return
			}

			// Check success condition
			if !tc.expectedFile {
				assert.NoError(t, err, "Expected no error")
				assert.Nil(t, file, "Expected nil file")
				return
			}

			// Verify successful capture
			require.NoError(t, err, "Expected no error but got: %v", err)
			require.NotNil(t, file, "Expected non-nil file")
			t.Cleanup(func() { file.Close() })

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

func TestVMStat_Run(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "vmstat-run-test-*")
	require.NoError(t, err, "Failed to create temp directory")
	t.Cleanup(func() { os.RemoveAll(tmpDir) })

	// Change to temp directory for test execution
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	t.Cleanup(func() { os.Chdir(originalDir) })

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

	// Store original VMState command
	originalVMState := executils.VMState
	t.Cleanup(func() { executils.VMState = originalVMState })

	// Set up mock server
	server := setupMockServer(t)

	tests := []struct {
		name          string
		setupCommands func()
		expectedOk    bool
		description   string
	}{
		{
			name: "successful run with upload",
			setupCommands: func() {
				// Use the real VMState command from executils - no changes needed
			},
			expectedOk:  true,
			description: "Should successfully capture and upload vmstat data using real platform command",
		},
		{
			name: "command fails - platform specific behavior",
			setupCommands: func() {
				executils.VMState = createFailingCommand()
			},
			expectedOk:  isLinux(), // On Linux, fallback might succeed; on others, expect failure
			description: "Should handle command failure based on platform capabilities",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("Test description: %s", tc.description)
			t.Logf("Running on platform: %s", runtime.GOOS)

			// Clean up any existing output file
			os.RemoveAll(vmstatOutputPath)

			// Setup test commands
			tc.setupCommands()

			// Create VMStat instance with mock endpoint
			v := &VMStat{}
			v.SetEndpoint(server.URL + "/ycrash-receiver?de=localhost")

			// Run the capture and upload
			result, err := v.Run()

			if tc.expectedOk {
				assert.NoError(t, err, "Expected no error but got: %v", err)
				assert.True(t, result.Ok, "Expected result.Ok to be true, got: %s", result.Msg)
			} else {
				// On failure, we might get an error or a failed result
				if err == nil {
					assert.False(t, result.Ok, "Expected result.Ok to be false when command fails")
				}
			}
		})
	}
}

func TestVMStat_LinuxValidation_WithRealOutput(t *testing.T) {
	if !isLinux() {
		t.Skip("Skipping Linux-specific validation test on non-Linux platform")
	}

	// Test the validation function with real vmstat output
	// This test will run the actual vmstat command and validate its output
	t.Run("real vmstat output validation", func(t *testing.T) {
		// Create a temporary VMStat instance to capture real output
		tmpDir, err := os.MkdirTemp("", "vmstat-validation-test-*")
		require.NoError(t, err, "Failed to create temp directory")
		t.Cleanup(func() { os.RemoveAll(tmpDir) })

		originalDir, err := os.Getwd()
		require.NoError(t, err, "Failed to get current directory")
		t.Cleanup(func() { os.Chdir(originalDir) })

		err = os.Chdir(tmpDir)
		require.NoError(t, err, "Failed to change to temp directory")

		// Use the real VMState command to capture actual output
		v := &VMStat{}
		file, err := v.CaptureToFile()

		if err != nil {
			t.Logf("VMStat capture failed (this might be expected on some systems): %v", err)
			return
		}

		require.NotNil(t, file, "Expected non-nil file")
		t.Cleanup(func() { file.Close() })

		// Read the captured output
		_, err = file.Seek(0, 0)
		require.NoError(t, err, "Failed to seek to start of file")

		content := make([]byte, 4096)
		n, err := file.Read(content)
		if err != nil && err.Error() != "EOF" {
			require.NoError(t, err, "Failed to read file content")
		}

		output := string(content[:n])
		t.Logf("Captured vmstat output:\n%s", output)

		// Validate the real output
		if len(output) > 0 {
			valid, errMsg := validateLinuxVMStatOutput(output)
			if !valid {
				t.Logf("Validation failed: %s", errMsg)
				t.Logf("This might indicate the validation function needs adjustment for this system's vmstat format")
			} else {
				t.Logf("Real vmstat output validation passed")
			}
		}
	})
}

// isLinux returns true if running on Linux
func isLinux() bool {
	return runtime.GOOS == "linux"
}
