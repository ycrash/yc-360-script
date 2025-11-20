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

func TestGetGlobPatternFromGCPath(t *testing.T) {
	tests := []struct {
		name     string
		gcPath   string
		pid      int
		expected string
	}{
		{
			name:     "basic pattern with %p and %t",
			gcPath:   "/tmp/buggyapp-%p-%t.log",
			pid:      1234,
			expected: "/tmp/buggyapp-*1234-????-??-??_??-??-??.log",
		},
		{
			name:     "pattern with %pid",
			gcPath:   "/tmp/buggyapp-%pid-%t.log",
			pid:      5678,
			expected: "/tmp/buggyapp-5678-????-??-??_??-??-??.log",
		},
		{
			name:     "pattern with multiple placeholders",
			gcPath:   "/logs/app-%Y-%m-%d_%H-%M-%S-%p.log",
			pid:      9999,
			expected: "/logs/app-????-??-??_??-??-??-*9999.log",
		},
		{
			name:     "pattern with %seq and %tick",
			gcPath:   "/tmp/gc-%seq-%tick-%p.log",
			pid:      1111,
			expected: "/tmp/gc-???-*-*1111.log",
		},
		{
			name:     "pattern with %uid and %last",
			gcPath:   "/var/log/gc-%uid-%last-%p.log",
			pid:      2222,
			expected: "/var/log/gc-*-*-*2222.log",
		},
		{
			name:     "no placeholders",
			gcPath:   "/tmp/static.log",
			pid:      3333,
			expected: "/tmp/static.log",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := GetGlobPatternFromGCPath(tc.gcPath, tc.pid)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestGetLatestFileFromGlobPattern(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name          string
		pattern       string
		files         []string
		expectedFile  string
		expectedError bool
	}{
		{
			name:         "single file match",
			pattern:      filepath.Join(tmpDir, "app-*.log"),
			files:        []string{"app-123.log"},
			expectedFile: "app-123.log",
		},
		{
			name:         "multiple files - returns latest alphabetically",
			pattern:      filepath.Join(tmpDir, "app-*.log"),
			files:        []string{"app-111.log", "app-222.log", "app-333.log"},
			expectedFile: "app-333.log", // Latest alphabetically
		},
		{
			name:         "complex pattern matching",
			pattern:      filepath.Join(tmpDir, "gc-*-*.log"),
			files:        []string{"gc-2023-10-28.log", "gc-2023-10-29.log", "gc-2023-11-01.log"},
			expectedFile: "gc-2023-11-01.log",
		},
		{
			name:          "no matching files",
			pattern:       filepath.Join(tmpDir, "nonexistent-*.log"),
			files:         []string{},
			expectedError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create test files
			for _, file := range tc.files {
				fullPath := filepath.Join(tmpDir, file)
				err := os.WriteFile(fullPath, []byte("test content"), 0644)
				require.NoError(t, err, "Failed to create test file %s", file)
			}

			// Test the function
			result, err := GetLatestFileFromGlobPattern(tc.pattern)

			if tc.expectedError {
				assert.Error(t, err)
				assert.Empty(t, result)
			} else {
				assert.NoError(t, err)
				assert.True(t, strings.HasSuffix(result, tc.expectedFile),
					"Expected result to end with %s, got %s", tc.expectedFile, result)
			}

			// Clean up test files
			for _, file := range tc.files {
				os.Remove(filepath.Join(tmpDir, file))
			}
		})
	}
}

func TestFindLatestFileInRotatingLogFiles(t *testing.T) {
	tmpDir := t.TempDir()
	baseFile := filepath.Join(tmpDir, "gc.log")

	tests := []struct {
		name         string
		setupFiles   []string
		expectedFile string
	}{
		{
			name:         "base file exists, no rotated files",
			setupFiles:   []string{"gc.log"},
			expectedFile: "gc.log",
		},
		{
			name:         "rotated files exist, return latest by modification time",
			setupFiles:   []string{"gc.log.1", "gc.log.2", "gc.log.3"},
			expectedFile: "gc.log.3", // Will be created last, so latest mod time
		},
		{
			name:         "base file and rotated files",
			setupFiles:   []string{"gc.log", "gc.log.1", "gc.log.2"},
			expectedFile: "gc.log.2", // Latest created file
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Create test files with slight time delays to ensure different mod times
			for i, file := range tc.setupFiles {
				fullPath := filepath.Join(tmpDir, file)
				err := os.WriteFile(fullPath, []byte("test content"), 0644)
				require.NoError(t, err, "Failed to create test file %s", file)

				// Add small delay to ensure different modification times
				if i < len(tc.setupFiles)-1 {
					time.Sleep(10 * time.Millisecond)
				}
			}

			result := findLatestFileInRotatingLogFiles(baseFile)
			expectedPath := filepath.Join(tmpDir, tc.expectedFile)
			assert.Equal(t, expectedPath, result)

			// Clean up
			for _, file := range tc.setupFiles {
				os.Remove(filepath.Join(tmpDir, file))
			}
		})
	}
}

func TestGC_Run_Integration(t *testing.T) {
	tmpDir := t.TempDir()
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	defer os.Chdir(originalDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

	tests := []struct {
		name     string
		setupGC  func() *GC
		expectOk bool
	}{
		{
			name: "GC with valid local file",
			setupGC: func() *GC {
				// Create a test GC log file
				gcLogPath := filepath.Join(tmpDir, "test-gc.log")
				content := `2023.10.28 09:07:59 GC 1 pause 10ms
2023.10.28 09:08:00 GC 2 pause 12ms`
				err := os.WriteFile(gcLogPath, []byte(content), 0644)
				require.NoError(t, err)

				return &GC{
					Pid:    1234,
					GCPath: gcLogPath,
				}
			},
			expectOk: true,
		},
		{
			name: "GC with pattern-based path",
			setupGC: func() *GC {
				// Create a file that matches a pattern
				gcLogPath := filepath.Join(tmpDir, "app-1234-2023-10-28_09-07-59.log")
				content := `GC log content with pattern`
				err := os.WriteFile(gcLogPath, []byte(content), 0644)
				require.NoError(t, err)

				return &GC{
					Pid:    1234,
					GCPath: filepath.Join(tmpDir, "app-%p-%t.log"),
				}
			},
			expectOk: true,
		},
		{
			name: "GC with no GC path - fallback to jstat",
			setupGC: func() *GC {
				return &GC{
					Pid:      1234,
					GCPath:   "",
					JavaHome: "/usr/lib/jvm/java-8-openjdk",
				}
			},
			expectOk: false, // Will likely fail in test environment without actual Java
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gc := tc.setupGC()
			gc.SetEndpoint("http://localhost:8080/test")

			result, err := gc.Run()

			// The result depends on whether PostData succeeds
			// In test environment, this will likely fail to connect
			// but we can still verify the file processing worked
			if tc.expectOk {
				assert.NoError(t, err, "Run should not return error for valid setup")
			}

			// Result message should contain GC Path information
			assert.Contains(t, result.Msg, "GC Path:", "Result should contain GC Path info")
		})
	}
}

func TestFileExists(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name     string
		setup    func() string
		expected bool
	}{
		{
			name: "existing file",
			setup: func() string {
				filePath := filepath.Join(tmpDir, "exists.txt")
				err := os.WriteFile(filePath, []byte("content"), 0644)
				require.NoError(t, err)
				return filePath
			},
			expected: true,
		},
		{
			name: "non-existent file",
			setup: func() string {
				return filepath.Join(tmpDir, "does-not-exist.txt")
			},
			expected: false,
		},
		{
			name: "directory instead of file",
			setup: func() string {
				dirPath := filepath.Join(tmpDir, "directory")
				err := os.Mkdir(dirPath, 0755)
				require.NoError(t, err)
				return dirPath
			},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.setup()
			result := fileExists(path)
			assert.Equal(t, tc.expected, result)
		})
	}
}

// Test helper function to create test files with specific modification times
func createTestFileWithModTime(t *testing.T, path string, content string, modTime time.Time) {
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err, "Failed to write test file")

	err = os.Chtimes(path, modTime, modTime)
	require.NoError(t, err, "Failed to set file modification time")
}

func TestFindLatestFileInRotatingLogFiles_WithSpecificModTimes(t *testing.T) {
	tmpDir := t.TempDir()
	baseFile := filepath.Join(tmpDir, "gc.log")

	// Create files with specific modification times
	now := time.Now()
	older := now.Add(-1 * time.Hour)
	oldest := now.Add(-2 * time.Hour)

	createTestFileWithModTime(t, baseFile, "base content", oldest)
	createTestFileWithModTime(t, baseFile+".1", "rotated content 1", older)
	createTestFileWithModTime(t, baseFile+".2", "rotated content 2", now)

	result := findLatestFileInRotatingLogFiles(baseFile)
	expected := baseFile + ".2"

	assert.Equal(t, expected, result, "Should return the file with latest modification time")
}

func TestProcessGCLogFile_RealFiles(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name           string
		gcPath         string
		setupFiles     func() string // Returns the actual gcPath to use
		expectedResult bool
		expectedError  bool
	}{
		{
			name:   "empty gcPath returns nil",
			gcPath: "",
			setupFiles: func() string {
				return ""
			},
			expectedResult: false,
			expectedError:  false,
		},
		{
			name:   "existing file",
			gcPath: filepath.Join(tmpDir, "gc.log"),
			setupFiles: func() string {
				gcPath := filepath.Join(tmpDir, "gc.log")
				content := `GC log content`
				err := os.WriteFile(gcPath, []byte(content), 0644)
				require.NoError(t, err)
				return gcPath
			},
			expectedResult: true,
			expectedError:  false,
		},
		{
			name:   "pattern-based path",
			gcPath: filepath.Join(tmpDir, "app-%p-%t.log"),
			setupFiles: func() string {
				// Create a file that matches the pattern
				matchingFile := filepath.Join(tmpDir, "app-1234-2023-10-28_09-07-59.log")
				content := `GC log content with pattern`
				err := os.WriteFile(matchingFile, []byte(content), 0644)
				require.NoError(t, err)
				return filepath.Join(tmpDir, "app-%p-%t.log")
			},
			expectedResult: true,
			expectedError:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			actualGCPath := tc.setupFiles()
			outFile := filepath.Join(tmpDir, "out-"+tc.name+".log")

			file, err := ProcessGCLogFile(actualGCPath, outFile, "", 1234)

			if tc.expectedError {
				assert.Error(t, err)
				assert.Nil(t, file)
			} else if tc.expectedResult {
				assert.NoError(t, err)
				assert.NotNil(t, file)
				file.Close()

				// Verify output file was created
				_, statErr := os.Stat(outFile)
				assert.NoError(t, statErr, "Output file should be created")
			} else {
				// expectedResult = false, expectedError = false (empty gcPath case)
				assert.NoError(t, err)
				assert.Nil(t, file)
			}
		})
	}
}
