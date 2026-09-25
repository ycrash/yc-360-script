package capture

import (
	"archive/zip"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIsMostlyASCII verifies that the IsMostlyASCII function correctly identifies
// text content as ASCII or non-ASCII based on a 70% threshold. The function should
// handle various scenarios including pure ASCII, mixed content, and edge cases.
func TestIsMostlyASCII(t *testing.T) {
	// Tests 100% ASCII content
	t.Run("AllASCII", func(t *testing.T) {
		// Setup: String with only ASCII characters
		data := []byte("Hello, World!")

		// Test: Check if content is mostly ASCII
		result := IsMostlyASCII(data)

		// Expect: Should return true as all characters are ASCII
		assert.True(t, result, "should identify pure ASCII content correctly")
	})

	// Tests content with equal ASCII and non-ASCII distribution
	t.Run("50PercentASCII", func(t *testing.T) {
		// Setup: 5 ASCII characters and 5 non-ASCII characters (Cyrillic)
		// ASCII: ABCDE (5 chars)
		// Non-ASCII: абвгд (5 chars)
		data := []byte("ABCDEабвгд")

		// Test: Check if content meets ASCII threshold
		result := IsMostlyASCII(data)

		// Expect: Should return false as 50% is below the 70% threshold
		assert.False(t, result, "should reject content with only 50% ASCII")
	})

	// Tests content exactly at the threshold boundary
	t.Run("Threshold70Percent", func(t *testing.T) {
		// Setup: 7 ASCII chars and 3 non-ASCII chars = 70% ASCII
		// ASCII: ABCDEFg (7 chars)
		// Non-ASCII: абв (3 chars)
		data := []byte("ABCDEFgабв")

		// Test: Check if content at threshold is handled correctly
		result := IsMostlyASCII(data)

		// Expect: Should return false as we need >70% (not >=70%)
		assert.False(t, result, "should reject content exactly at 70% threshold")
	})

	// Tests empty input handling
	t.Run("EmptySlice", func(t *testing.T) {
		// Setup: Empty byte slice
		data := []byte{}

		// Test and Expect: Function should not panic
		assert.NotPanics(t, func() {
			IsMostlyASCII(data)
		}, "should handle empty input without panicking")
	})
}

// TestMatchLogPattern verifies that log file patterns are correctly matched based on filename.
// It tests various filename scenarios.
func TestMatchLogPattern(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"example.log", true},           // standard .log file
		{"example-rotated.log.1", true}, // rotated log file matches second pattern (.*log.*\..*)
		{"mylog.txt", true},             // file containing 'log' with extension matches second pattern
		{"logfile", false},              // no match: missing extension (no dot after 'log')
		{"output.LOG", false},           // no match: case-sensitive patterns
		{"test.txt", false},             // no match: not a log file
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := matchLogPattern(tt.filename)
			assert.Equal(t, tt.expected, got,
				"matchLogPattern(%q): got %v, want %v",
				tt.filename, got, tt.expected)
		})
	}
}

// TestIsLogFileName verifies that library jars and other binary/archive files are rejected
// even when their names match a log pattern.
func TestIsLogFileName(t *testing.T) {
	tests := []struct {
		filename string
		expected bool
	}{
		{"example.log", true},
		{"example-rotated.log.1", true},
		{"mylog.txt", true},
		{"log4j-core-2.19.0.jar", false},      // matches *log*.*, rejected by extension
		{"logback-classic-1.2.11.jar", false}, // matches *log*.*, rejected by extension
		{"commons-logging-1.2.jar", false},    // matches *log*.*, rejected by extension
		{"jboss-logging-3.5.0.jar", false},    // matches *log*.*, rejected by extension
		{"LOG4J-CORE-2.19.0.JAR", false},      // log patterns are case-sensitive: never reaches the extension check
		{"log4j-core-2.19.0.JAR", false},      // extension check is case-insensitive
		{"slf4j-api-1.7.36.jar", false},       // no "log" substring
		{"app.log.gz", false},                 // compressed rotated logs are never auto-discovered
		{"catalog.zip", false},
		{"logging.so", false},
	}

	for _, tt := range tests {
		t.Run(tt.filename, func(t *testing.T) {
			got := isLogFileName(tt.filename)
			assert.Equal(t, tt.expected, got,
				"isLogFileName(%q): got %v, want %v",
				tt.filename, got, tt.expected)
		})
	}
}

func buildZip(t *testing.T, comment string) []byte {
	t.Helper()

	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("org/apache/logging/log4j/core/appender/rolling/action/AbstractPathAction$DeletingVisitorWithVeryLongName%03d.class", i)
		f, err := w.Create(name)
		require.NoError(t, err)
		_, err = f.Write([]byte("class content"))
		require.NoError(t, err)
	}
	require.NoError(t, w.SetComment(comment))
	require.NoError(t, w.Close())

	return buf.Bytes()
}

func headAndTail(b []byte) ([]byte, []byte) {
	return b[:min(int(headSampleLen), len(b))], b[max(0, len(b)-tailSampleLen):]
}

// TestLooksLikeTextLog verifies that archives are rejected by content even when their tail
// passes the ASCII threshold, while genuine logs are accepted.
func TestLooksLikeTextLog(t *testing.T) {
	t.Run("TextLog", func(t *testing.T) {
		head, tail := headAndTail([]byte("2026-09-22 14:45:23 INFO  Application started\n"))
		assert.True(t, looksLikeTextLog(head, tail))
	})

	// copytruncate rotation leaves a NUL run at the head of a genuine log
	t.Run("TextLogWithNULHead", func(t *testing.T) {
		content := append(make([]byte, 4096), []byte(strings.Repeat("2026-09-22 14:45:23 INFO  request served\n", 50))...)
		head, tail := headAndTail(content)
		assert.True(t, looksLikeTextLog(head, tail), "NUL bytes at the head must not reject a log")
	})

	// A few stray NULs in a log line must not hide the log
	t.Run("TextLogWithStrayNULs", func(t *testing.T) {
		var content []byte
		for i := 0; i < 10; i++ {
			content = append(content, "2026-09-25 10:00:00 INFO  [exec-1] c.e.OrderController - order 4711 served in 12 ms\n"...)
		}
		content = append(content, "2026-09-25 10:00:20 ERROR [exec-2] c.e.ReportController - download failed: Nul character not allowed: invoice.pdf\x00.jsp\n"...)
		content = append(content, "2026-09-25 10:00:21 WARN  [mq-1] c.e.PaymentListener - unknown account ACC-0042\x00\x00\x00\x00\n"...)
		for i := 0; i < 5; i++ {
			content = append(content, "2026-09-25 10:00:22 INFO  [exec-1] c.e.OrderController - order 4712 served in 11 ms\n"...)
		}
		head, tail := headAndTail(content)
		require.Equal(t, 5, bytes.Count(tail, []byte{0}), "precondition: the sampled tail holds the five stray NULs")
		assert.True(t, looksLikeTextLog(head, tail), "a few stray NUL bytes must not reject a log")
	})

	// log4j-core-2.19.0.jar: tail passes the ASCII check, head signature rejects it
	t.Run("Jar", func(t *testing.T) {
		head, tail := headAndTail(buildZip(t, ""))
		require.True(t, IsMostlyASCII(tail), "precondition: the jar tail must pass the ASCII check")
		require.True(t, hasManyNULs(tail), "precondition: the jar tail must be dense with NUL bytes")
		assert.False(t, looksLikeTextLog(head, tail))
	})

	// ASCII archive comment hides the NULs: only the head signature rejects it
	t.Run("ZipWithLongASCIIComment", func(t *testing.T) {
		head, tail := headAndTail(buildZip(t, strings.Repeat("A", 2000)))
		require.NotContains(t, string(tail), "\x00", "precondition: the tail must contain no NUL byte")
		require.True(t, IsMostlyASCII(tail), "precondition: the tail must pass the ASCII check")
		assert.False(t, looksLikeTextLog(head, tail))
	})

	// Executable jar starts with a script: only the tail NULs reject it
	t.Run("ExecutableJar", func(t *testing.T) {
		content := append([]byte("#!/bin/bash\nexec java -jar \"$0\" \"$@\"\n"), buildZip(t, "")...)
		head, tail := headAndTail(content)
		require.False(t, hasBinarySignature(head), "precondition: the head must not carry a signature")
		require.True(t, IsMostlyASCII(tail), "precondition: the tail must pass the ASCII check")
		require.True(t, hasManyNULs(tail), "precondition: the tail must be dense with NUL bytes")
		assert.False(t, looksLikeTextLog(head, tail))
	})

	t.Run("Empty", func(t *testing.T) {
		assert.False(t, looksLikeTextLog(nil, nil))
	})
}

func TestHasManyNULs(t *testing.T) {
	sample := func(size, nuls int) []byte {
		b := bytes.Repeat([]byte{'a'}, size)
		for i := 0; i < nuls; i++ {
			b[i] = 0
		}
		return b
	}

	tests := []struct {
		name     string
		size     int
		nuls     int
		expected bool
	}{
		{"NoNUL", 1000, 0, false},
		{"AtThreshold", 1000, 10, false},
		{"AboveThreshold", 1000, 11, true},
		{"SmallSampleOneNUL", 50, 1, true}, // 2% of a short file
		{"Empty", 0, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, hasManyNULs(sample(tt.size, tt.nuls)))
		})
	}
}

func TestHeadSampleLenCoversSignatures(t *testing.T) {
	for _, signature := range binaryFileSignatures {
		file := append(append([]byte{}, signature...), bytes.Repeat([]byte{'x'}, 16)...)
		head, _ := headAndTail(file)
		assert.True(t, hasBinarySignature(head), "signature % x must fit in a %d-byte head sample", signature, headSampleLen)
	}
}

func TestHasBinarySignature(t *testing.T) {
	tests := []struct {
		name     string
		head     []byte
		expected bool
	}{
		{"Zip", []byte{'P', 'K', 0x03, 0x04}, true},
		{"Gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, true},
		{"ELF", []byte{0x7f, 'E', 'L', 'F'}, true},
		{"MachO64", []byte{0xcf, 0xfa, 0xed, 0xfe}, true},
		{"ClassFile", []byte{0xca, 0xfe, 0xba, 0xbe}, true},
		{"Text", []byte("2026"), false},
		{"NULRun", []byte{0, 0, 0, 0}, false},
		{"TooShort", []byte{'P', 'K'}, false},
		{"Empty", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, hasBinarySignature(tt.head))
		})
	}
}

func TestSampleFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0644))
		return path
	}

	t.Run("LargerThanSamples", func(t *testing.T) {
		head, tail, err := sampleFile(write("large.log", "Hello, World!"), 5, 6)
		require.NoError(t, err)
		assert.Equal(t, "Hello", string(head))
		assert.Equal(t, "World!", string(tail))
	})

	t.Run("SmallerThanSamples", func(t *testing.T) {
		head, tail, err := sampleFile(write("small.log", "Hello"), 10, 10)
		require.NoError(t, err)
		assert.Equal(t, "Hello", string(head), "the whole file when it is shorter than the head sample")
		assert.Equal(t, "Hello", string(tail), "the whole file when it is shorter than the tail sample")
	})

	t.Run("ExactSize", func(t *testing.T) {
		head, tail, err := sampleFile(write("exact.log", "1234567890"), 10, 10)
		require.NoError(t, err)
		assert.Equal(t, "1234567890", string(head))
		assert.Equal(t, "1234567890", string(tail))
	})

	t.Run("Empty", func(t *testing.T) {
		head, tail, err := sampleFile(write("empty.log", ""), 4, 1000)
		require.NoError(t, err)
		assert.Empty(t, head)
		assert.Empty(t, tail)
	})

	t.Run("NonExistentFile", func(t *testing.T) {
		_, _, err := sampleFile(filepath.Join(dir, "no_such_file.log"), 4, 1000)
		assert.Error(t, err)
	})
}

func TestReadTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shrunk.log")
	content := []byte(strings.Repeat("2026-09-25 INFO request served\n", 40)) // 1240 bytes
	require.NoError(t, os.WriteFile(path, content, 0644))
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	// Measured at 1440 bytes but truncated by 200: only 800 of 1000 bytes exist
	got, err := readTail(f, int64(len(content)+200), 1000)
	require.NoError(t, err)
	assert.Equal(t, string(content[440:]), string(got), "must return exactly the bytes read")
	assert.NotContains(t, string(got), "\x00", "must not pad the tail with zero bytes")

	got, err = readTail(f, int64(len(content)), 1000)
	require.NoError(t, err)
	assert.Equal(t, string(content[240:]), string(got), "unchanged file: the last 1000 bytes")
}

func TestDiscoverOpenedLogFilesByProcess(t *testing.T) {
	// Skip test on non-Linux platforms since the functionality is Linux-specific
	if runtime.GOOS != "linux" {
		return
	}

	// Test setup
	pid := os.Getpid()
	dir, err := os.MkdirTemp("", "discoverLogsTest")
	require.NoError(t, err, "failed to create temp directory")
	defer os.RemoveAll(dir)

	// Test cases define different types of files we want to verify
	// Each case tests a specific aspect of log file detection
	testCases := []struct {
		name           string // Name of the test file
		content        []byte // Content to write to the file
		shouldBeASCII  bool   // Whether content meets ASCII threshold
		shouldMatchLog bool   // Whether filename matches log patterns
	}{
		{
			name:           "test1.log",
			content:        []byte("This is a mostly ASCII log file."),
			shouldBeASCII:  true,
			shouldMatchLog: true,
		},
		{
			name:           "test2.log",
			content:        []byte("абвгд Пример не-ASCII"),
			shouldBeASCII:  false,
			shouldMatchLog: true,
		},
		{
			name:           "test3.txt",
			content:        []byte("Some ASCII content but not a .log"),
			shouldBeASCII:  true,
			shouldMatchLog: false,
		},
		{
			name:           "testlog.out",
			content:        []byte("Another ASCII log-like file."),
			shouldBeASCII:  true,
			shouldMatchLog: true,
		},
		{
			// Library jar whose tail passes the ASCII check: rejected by extension
			name:           "log4j-core-2.19.0.jar",
			content:        []byte("org/apache/logging/log4j/core/appender/FileAppender.class"),
			shouldBeASCII:  true,
			shouldMatchLog: false,
		},
		{
			// Compressed rotated log whose tail passes the ASCII check: rejected by extension
			name:           "app.log.gz",
			content:        []byte("A stored gzip can look like plain text in its tail."),
			shouldBeASCII:  true,
			shouldMatchLog: false,
		},
		{
			// Zip under a name without a binary extension: rejected by content
			name:           "backup-log.bak",
			content:        buildZip(t, ""),
			shouldBeASCII:  false,
			shouldMatchLog: true,
		},
	}

	// Create and open all test files
	var openedFiles []*os.File
	defer func() {
		// Cleanup: close all opened files
		for _, f := range openedFiles {
			if f != nil {
				f.Close()
			}
		}
	}()

	// Set up test files and keep them open
	for _, tc := range testCases {
		fullPath := filepath.Join(dir, tc.name)

		// Create and write content
		err := os.WriteFile(fullPath, tc.content, 0644)
		require.NoError(t, err, "failed to create and write to file %q", tc.name)

		// Keep file open for detection
		f, err := os.Open(fullPath)
		require.NoError(t, err, "failed to open file %q", tc.name)
		openedFiles = append(openedFiles, f)
	}

	// Second descriptor to the first log file
	duplicatePath := filepath.Join(dir, testCases[0].name)
	duplicate, err := os.Open(duplicatePath)
	require.NoError(t, err, "failed to open file %q a second time", testCases[0].name)
	openedFiles = append(openedFiles, duplicate)

	// Run the discovery function
	discoveredFiles, err := DiscoverOpenedLogFilesByProcess(pid)
	require.NoError(t, err, "DiscoverOpenedLogFilesByProcess failed")

	// Convert results to a map for easier verification
	discoveredSet := make(map[string]bool)
	discoveredCount := make(map[string]int)
	for _, path := range discoveredFiles {
		discoveredSet[path] = true
		discoveredCount[path]++
	}

	// A file opened through several descriptors must be discovered once
	assert.Equal(t, 1, discoveredCount[duplicatePath],
		"file %q: opened twice, expected to be discovered once", testCases[0].name)

	// Verify each test case
	for _, tc := range testCases {
		fullPath := filepath.Join(dir, tc.name)

		// A file should be discovered only if it matches both conditions:
		// 1. Filename matches log pattern
		// 2. Content is mostly ASCII
		expectedDiscovery := tc.shouldMatchLog && tc.shouldBeASCII
		actuallyDiscovered := discoveredSet[fullPath]

		assert.Equal(t, expectedDiscovery, actuallyDiscovered,
			"file %q: unexpected discovery status (expected=%v, actual=%v)",
			tc.name, expectedDiscovery, actuallyDiscovered)
	}
}
