package capture

import (
	"bytes"
	"encoding/binary"
	"os"
	"strings"
	"testing"
	"unicode/utf16"

	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewAppLogM3 verifies that a new instance is properly initialized.
func TestNewAppLogM3(t *testing.T) {
	appLog := NewAppLogM3()
	assert.NotNil(t, appLog.readStats, "readStats should be initialized")
	assert.NotNil(t, appLog.Paths, "Paths should be initialized")
}

// TestCaptureSingleAppLog_Initialization tests that on first encounter the file's read position is initialized.
func TestAppLogM3_CaptureSingleAppLog_Initialization(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	// Given
	filename := "test.log"
	content := "line1\nline2\n"
	require.NoError(t, os.WriteFile(filename, []byte(content), 0644))

	// When
	appLog := NewAppLogM3()

	// Call CaptureSingleAppLog for the first time.
	result, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)
	assert.Contains(t, result.Msg, "initialized read position", "should indicate initialization")
	assert.True(t, result.Ok)

	// Verify that the readStats has been set to the current file size.
	fi, err := os.Stat(filename)
	require.NoError(t, err)
	stat := appLog.readStats[filename]
	assert.Equal(t, fi.Size(), stat.readPosition, "read position should equal file size")
	assert.Equal(t, fi.Size(), stat.fileSize, "fileSize should equal file size")

	// Since this was initialization, no destination file should be created.
	files, err := os.ReadDir(tmpDir)
	require.NoError(t, err)

	// There should be only one file ("test.log").
	assert.Len(t, files, 1, "only the source file should exist on initialization")
}

// TestCaptureSingleAppLog_ReadNewContent tests that appended log content is captured.
func TestAppLogM3_CaptureSingleAppLog_ReadNewContent(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	filename := "test.log"
	initialContent := "line1\n"
	require.NoError(t, os.WriteFile(filename, []byte(initialContent), 0644))

	appLog := NewAppLogM3()

	// First call initializes the read position.
	res, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)
	assert.Contains(t, res.Msg, "initialized read position")

	// Append new content.
	newContent := "line2\n"
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0644)
	require.NoError(t, err)
	_, err = f.WriteString(newContent)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	// Capture list of files before the second call.
	filesBefore, err := os.ReadDir(tmpDir)
	require.NoError(t, err)

	// Second call should copy only the appended content.
	_, err = appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	// Find the new destination file created.
	filesAfter, err := os.ReadDir(tmpDir)
	require.NoError(t, err)

	// Exclude the source file.
	var destFiles []os.DirEntry
	for _, f := range filesAfter {
		if f.Name() != filename {
			destFiles = append(destFiles, f)
		}
	}

	// Expect one new file to be created.
	assert.Len(t, destFiles, len(filesAfter)-len(filesBefore), "should create one destination file")

	// Read the content of the destination file.
	destPath := destFiles[0].Name()
	data, err := os.ReadFile(destPath)
	require.NoError(t, err)

	// Since we appended "line2\n", that should be the content copied.
	assert.Equal(t, newContent, string(data), "destination file should contain only the new appended content")

	// Verify that the read position is updated (should equal total file size).
	fi, err := os.Stat(filename)
	require.NoError(t, err)
	stat := appLog.readStats[filename]
	assert.Equal(t, fi.Size(), stat.readPosition, "read position should equal current file size")
}

// TestCaptureSingleAppLog_Truncated tests that when the source log file is truncated the read position is reset.
func TestCaptureSingleAppLog_Truncated(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	// Write initial content
	filename := "test.log"
	initialContent := "line1\nline2\n"
	require.NoError(t, os.WriteFile(filename, []byte(initialContent), 0644))

	appLog := NewAppLogM3()

	// First call initializes the read position.
	res, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)
	assert.Contains(t, res.Msg, "initialized read position")

	// Now simulate truncation (for example, due to log rotation) by writing a shorter file.
	truncatedContent := "new line\n"
	require.NoError(t, os.WriteFile(filename, []byte(truncatedContent), 0644))

	// Second call should detect truncation and reset the read position to 0, then copy the new content.
	_, err = appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	// Find the new destination file.
	entries, err := os.ReadDir(tmpDir)
	require.NoError(t, err)
	var destFile string
	for _, entry := range entries {
		if entry.Name() != filename {
			// We assume the destination file name is not equal to the source file.
			destFile = entry.Name()
			break
		}
	}
	require.NotEmpty(t, destFile, "a destination file should be created after truncation")
	data, err := os.ReadFile(destFile)
	require.NoError(t, err)
	assert.Equal(t, truncatedContent, string(data), "destination file should contain the entire new content")

	// Verify that the readStats is updated to the new file size.
	fi, err := os.Stat(filename)
	require.NoError(t, err)
	stat := appLog.readStats[filename]
	assert.Equal(t, fi.Size(), stat.readPosition, "read position should equal new file size")
}

// TestCaptureSingleAppLog_FileNotFound tests behavior when the source log file does not exist.
func TestCaptureSingleAppLog_FileNotFound(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	appLog := NewAppLogM3()

	nonExistent := "nonexistent.log"
	result, err := appLog.CaptureSingleAppLog(nonExistent, 123)
	require.Error(t, err, "should return an error when file does not exist")
	assert.Contains(t, err.Error(), "failed to open applog", "error should mention open failure")

	// In error case, the returned result is empty.
	assert.Empty(t, result.Msg)
}

// TestRun verifies that Run processes log files matching the provided glob pattern.
func TestAppLogM3_Run(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	// Create two test log files.
	file1 := "a.log"
	file2 := "b.log"
	content1 := "contentA\n"
	content2 := "contentB\n"
	require.NoError(t, os.WriteFile(file1, []byte(content1), 0644))
	require.NoError(t, os.WriteFile(file2, []byte(content2), 0644))

	appLog := NewAppLogM3()

	// Set the Paths field with a glob pattern that matches "*.log".
	appLog.Paths = map[int]config.AppLogs{
		1000: {"*.log"},
	}

	// Run the capture.
	result, err := appLog.Run()
	require.NoError(t, err)

	// Since these files are encountered for the first time, they should be initialized.
	assert.Contains(t, result.Msg, file1, "result message should mention first log file")
	assert.Contains(t, result.Msg, file2, "result message should mention second log file")
	assert.True(t, result.Ok, "result should indicate success")
}

// TestRun_InvalidGlob tests that an invalid glob pattern is handled appropriately.
func TestRun_InvalidGlob(t *testing.T) {
	// Create a temporary directory and change working directory into it.
	tmpDir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	defer os.Chdir(origDir)
	require.NoError(t, os.Chdir(tmpDir))

	appLog := NewAppLogM3()
	// Use an invalid glob pattern.
	appLog.Paths = map[int]config.AppLogs{
		1000: {"["},
	}

	result, err := appLog.Run()
	// A path that matches nothing is reported in the result, not as an error.
	require.NoError(t, err)
	assert.False(t, result.Ok, "result should indicate failure")
	assert.Contains(t, result.Msg, "doesn't exist", "zglob looks up \"[\" as a single file")
}

// TestAppLogM3_CaptureSingleAppLog_HoldsBackUnfinishedLine checks that an unfinished line goes up whole in the next run.
func TestAppLogM3_CaptureSingleAppLog_HoldsBackUnfinishedLine(t *testing.T) {
	t.Chdir(t.TempDir())
	filename := "test.log"
	require.NoError(t, os.WriteFile(filename, []byte("Inserted 1000 Records to map!\n"), 0644))

	appLog := NewAppLogM3()
	_, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	appendToFile(t, filename, "java.lang.OutOfMemoryError: Java heap space\nApplication is s")
	assert.Equal(t, "java.lang.OutOfMemoryError: Java heap space\n", captureAppLogM3Upload(t, appLog, filename))

	appendToFile(t, filename, "till running :-)\n")
	assert.Equal(t, "Application is still running :-)\n", captureAppLogM3Upload(t, appLog, filename))
}

// TestAppLogM3_CaptureSingleAppLog_SendsUnfinishedLineWhenIdle checks that an unfinished line goes up once the file stops growing.
func TestAppLogM3_CaptureSingleAppLog_SendsUnfinishedLineWhenIdle(t *testing.T) {
	t.Chdir(t.TempDir())
	filename := "test.log"
	require.NoError(t, os.WriteFile(filename, []byte("line1\n"), 0644))

	appLog := NewAppLogM3()
	_, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	appendToFile(t, filename, "unfinished")
	assert.Empty(t, captureAppLogM3Upload(t, appLog, filename), "the unfinished line waits while the file grows")
	assert.Equal(t, "unfinished", captureAppLogM3Upload(t, appLog, filename), "the unfinished line goes up when the file is idle")
}

// TestAppLogM3_CaptureSingleAppLog_InitializesBeforeUnfinishedLine checks that the first upload starts with a whole line.
func TestAppLogM3_CaptureSingleAppLog_InitializesBeforeUnfinishedLine(t *testing.T) {
	t.Chdir(t.TempDir())
	filename := "test.log"
	require.NoError(t, os.WriteFile(filename, []byte("line1\npart"), 0644))

	appLog := NewAppLogM3()
	_, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)
	assert.Equal(t, int64(len("line1\n")), appLog.readStats[filename].readPosition)

	appendToFile(t, filename, "ial\n")
	assert.Equal(t, "partial\n", captureAppLogM3Upload(t, appLog, filename))
}

// TestAppLogM3_CaptureSingleAppLog_DetectsReplacedFile checks that a rotated-in file is read from the start, even when it is larger.
func TestAppLogM3_CaptureSingleAppLog_DetectsReplacedFile(t *testing.T) {
	t.Chdir(t.TempDir())
	filename := "test.log"
	require.NoError(t, os.WriteFile(filename, []byte("old\n"), 0644))

	appLog := NewAppLogM3()
	_, err := appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	// Rename keeps the old file around, so the new one can't reuse its inode.
	require.NoError(t, os.Rename(filename, filename+".1"))
	require.NoError(t, os.WriteFile(filename, []byte("new line 1\nnew line 2\n"), 0644))

	assert.Equal(t, "new line 1\nnew line 2\n", captureAppLogM3Upload(t, appLog, filename))
}

// TestAppLogM3UploadEnd checks where an upload stops so that it ends with a complete line.
func TestAppLogM3UploadEnd(t *testing.T) {
	long := strings.Repeat("x", appLogM3MaxHeldBack)
	le := func(s string) []byte { return utf16Bytes(binary.LittleEndian, "\uFEFF"+s) }
	be := func(s string) []byte { return utf16Bytes(binary.BigEndian, "\uFEFF"+s) }

	tests := []struct {
		name     string
		data     []byte
		start    int64
		holdBack bool
		want     int64
	}{
		{"ends with a complete line", []byte("a\nb\n"), 0, true, 4},
		{"holds back an unfinished line", []byte("a\nApplication is s"), 0, true, 2},
		{"holds back everything without a newline", []byte("a\nunfinished"), 2, true, 2},
		{"sends the unfinished line when idle", []byte("a\nunfinished"), 2, false, 12},
		{"keeps CRLF together", []byte("a\r\nb\r\nc"), 0, true, 6},
		{"finds a newline several scan chunks back", []byte("a\n" + strings.Repeat("x", 200_000)), 0, true, 2},
		{"holds back up to the size limit", []byte("a\n" + long), 0, true, 2},
		{"sends a line past the size limit", []byte("a\n" + long + "x"), 0, true, int64(len(long)) + 3},
		{"UTF-16LE", le("a\r\nb"), 0, true, int64(len(le("a\r\n")))},
		{"UTF-16LE skips a newline byte inside another character", le("a\nੁ一"), 0, true, int64(len(le("a\n")))},
		{"UTF-16BE", be("a\nb"), 0, true, int64(len(be("a\n")))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := appLogM3UploadEnd(bytes.NewReader(tt.data), tt.start, int64(len(tt.data)), tt.holdBack)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// captureAppLogM3Upload runs one capture of filename and returns the content it uploaded.
func captureAppLogM3Upload(t *testing.T, appLog *AppLogM3, filename string) string {
	t.Helper()
	before, err := os.ReadDir(".")
	require.NoError(t, err)

	_, err = appLog.CaptureSingleAppLog(filename, 123)
	require.NoError(t, err)

	after, err := os.ReadDir(".")
	require.NoError(t, err)

	seen := make(map[string]bool, len(before))
	for _, e := range before {
		seen[e.Name()] = true
	}
	for _, e := range after {
		if !seen[e.Name()] {
			data, err := os.ReadFile(e.Name())
			require.NoError(t, err)
			return string(data)
		}
	}
	require.Fail(t, "capture wrote no upload file")
	return ""
}

// utf16Bytes encodes s as UTF-16 in the given byte order.
func utf16Bytes(order binary.AppendByteOrder, s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = order.AppendUint16(b, u)
	}
	return b
}
