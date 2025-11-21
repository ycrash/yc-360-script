package capture

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestVMStat_CaptureToFile(t *testing.T) {
	t.Run("file creation error when path is a directory", func(t *testing.T) {
		// Create temporary directory for test execution
		tmpDir := t.TempDir()

		// Change to temp directory for test execution
		originalDir, err := os.Getwd()
		require.NoError(t, err, "Failed to get current directory")
		defer os.Chdir(originalDir)

		err = os.Chdir(tmpDir)
		require.NoError(t, err, "Failed to change to temp directory")

		// Create a directory with the same name as output file to cause creation error
		err = os.Mkdir(vmstatOutputPath, 0755)
		require.NoError(t, err, "Failed to create blocking directory")

		// Run the capture - should fail because vmstat.out is a directory
		v := &VMStat{}
		file, err := v.CaptureToFile()

		// Should fail with file creation error
		assert.Error(t, err)
		assert.Nil(t, file)
		assert.Contains(t, err.Error(), "failed to create output file")
	})

	t.Run("creates output file in correct location", func(t *testing.T) {
		// Create temporary directory for test execution
		tmpDir := t.TempDir()

		// Change to temp directory for test execution
		originalDir, err := os.Getwd()
		require.NoError(t, err, "Failed to get current directory")
		defer os.Chdir(originalDir)

		err = os.Chdir(tmpDir)
		require.NoError(t, err, "Failed to change to temp directory")

		// Run the capture - this will run the actual vmstat command
		// On CI without vmstat, this may fail, but we're testing file creation
		v := &VMStat{}
		file, err := v.CaptureToFile()

		// The vmstat command may not be available in all environments
		// If it succeeds, verify the file properties
		if err == nil {
			require.NotNil(t, file)
			defer file.Close()

			// Verify correct file name
			assert.Equal(t, "vmstat.out", filepath.Base(file.Name()))

			// Verify the file was created
			_, statErr := os.Stat(vmstatOutputPath)
			assert.NoError(t, statErr, "Output file should exist")
		}
	})
}

func TestValidateLinuxVMStatOutput(t *testing.T) {
	t.Run("valid vmstat output", func(t *testing.T) {
		// Simulated valid vmstat output with 2 header lines + 5 data lines
		validOutput := `procs -----------memory---------- ---swap-- -----io---- -system-- ------cpu-----
 r  b   swpd   free   buff  cache   si   so    bi    bo   in   cs us sy id wa st
 0  0      0 123456  78901 234567    0    0     1     2   10   20  1  1 98  0  0
 0  0      0 123457  78902 234568    0    0     0     1   11   21  1  0 99  0  0
 0  0      0 123458  78903 234569    0    0     0     0   12   22  0  1 99  0  0
 0  0      0 123459  78904 234570    0    0     0     1   13   23  1  0 99  0  0
 0  0      0 123460  78905 234571    0    0     0     0   14   24  0  0 100  0  0`

		valid, errMsg := validateLinuxVMStatOutput(validOutput)
		assert.True(t, valid, "Expected valid output to pass validation")
		assert.Empty(t, errMsg, "Expected no error message for valid output")
	})

	t.Run("wrong number of lines", func(t *testing.T) {
		// Only 3 lines instead of expected 7 (2 headers + 5 data)
		invalidOutput := `procs -----------memory---------- ---swap--
 r  b   swpd   free   buff  cache
 0  0      0 123456  78901 234567`

		valid, errMsg := validateLinuxVMStatOutput(invalidOutput)
		assert.False(t, valid, "Expected invalid output to fail validation")
		assert.Contains(t, errMsg, "Expected 7 lines")
	})

	t.Run("missing memory header", func(t *testing.T) {
		// First line doesn't contain "-memory-"
		invalidOutput := `procs -----------other---------- ---swap-- -----io---- -system-- ------cpu-----
 r  b   swpd   free   buff  cache   si   so    bi    bo   in   cs us sy id wa st
 0  0      0 123456  78901 234567    0    0     1     2   10   20  1  1 98  0  0
 0  0      0 123457  78902 234568    0    0     0     1   11   21  1  0 99  0  0
 0  0      0 123458  78903 234569    0    0     0     0   12   22  0  1 99  0  0
 0  0      0 123459  78904 234570    0    0     0     1   13   23  1  0 99  0  0
 0  0      0 123460  78905 234571    0    0     0     0   14   24  0  0 100  0  0`

		valid, errMsg := validateLinuxVMStatOutput(invalidOutput)
		assert.False(t, valid, "Expected invalid output to fail validation")
		assert.Contains(t, errMsg, "-memory-")
	})

	t.Run("missing free column header", func(t *testing.T) {
		// Second line doesn't contain "free"
		invalidOutput := `procs -----------memory---------- ---swap-- -----io---- -system-- ------cpu-----
 r  b   swpd   available   buff  cache   si   so    bi    bo   in   cs us sy id wa st
 0  0      0 123456  78901 234567    0    0     1     2   10   20  1  1 98  0  0
 0  0      0 123457  78902 234568    0    0     0     1   11   21  1  0 99  0  0
 0  0      0 123458  78903 234569    0    0     0     0   12   22  0  1 99  0  0
 0  0      0 123459  78904 234570    0    0     0     1   13   23  1  0 99  0  0
 0  0      0 123460  78905 234571    0    0     0     0   14   24  0  0 100  0  0`

		valid, errMsg := validateLinuxVMStatOutput(invalidOutput)
		assert.False(t, valid, "Expected invalid output to fail validation")
		assert.Contains(t, errMsg, "free")
	})

	t.Run("missing buff column header", func(t *testing.T) {
		// Second line doesn't contain "buff"
		invalidOutput := `procs -----------memory---------- ---swap-- -----io---- -system-- ------cpu-----
 r  b   swpd   free   other  cache   si   so    bi    bo   in   cs us sy id wa st
 0  0      0 123456  78901 234567    0    0     1     2   10   20  1  1 98  0  0
 0  0      0 123457  78902 234568    0    0     0     1   11   21  1  0 99  0  0
 0  0      0 123458  78903 234569    0    0     0     0   12   22  0  1 99  0  0
 0  0      0 123459  78904 234570    0    0     0     1   13   23  1  0 99  0  0
 0  0      0 123460  78905 234571    0    0     0     0   14   24  0  0 100  0  0`

		valid, errMsg := validateLinuxVMStatOutput(invalidOutput)
		assert.False(t, valid, "Expected invalid output to fail validation")
		assert.Contains(t, errMsg, "buff")
	})

	t.Run("empty data line", func(t *testing.T) {
		// One of the data lines is empty
		invalidOutput := `procs -----------memory---------- ---swap-- -----io---- -system-- ------cpu-----
 r  b   swpd   free   buff  cache   si   so    bi    bo   in   cs us sy id wa st
 0  0      0 123456  78901 234567    0    0     1     2   10   20  1  1 98  0  0

 0  0      0 123458  78903 234569    0    0     0     0   12   22  0  1 99  0  0
 0  0      0 123459  78904 234570    0    0     0     1   13   23  1  0 99  0  0
 0  0      0 123460  78905 234571    0    0     0     0   14   24  0  0 100  0  0`

		valid, errMsg := validateLinuxVMStatOutput(invalidOutput)
		assert.False(t, valid, "Expected invalid output to fail validation")
		assert.Contains(t, errMsg, "empty")
	})
}
