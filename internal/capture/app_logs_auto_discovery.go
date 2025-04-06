package capture

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"unicode"
)

// logPatterns contains precompiled regex patterns so they are compiled once at startup rather than each function call.
var logPatterns = []*regexp.Regexp{
	regexp.MustCompile(`.*\.log$`),    // Matches *.log
	regexp.MustCompile(`.*log.*\..*`), // Matches *log*.*
}

// DiscoverOpenedLogFilesByProcess returns a list of file paths for log files that are
// opened by the given process identified by pid. A file is considered a log file if:
// - its name matches any of the precompiled log patterns,
// - and if its last 1000 bytes contain mostly ASCII characters.
//
// If the runtime is not Linux, it returns an empty slice with no error.
func DiscoverOpenedLogFilesByProcess(pid int) ([]string, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return []string{}, nil
	}

	openedFiles, err := GetOpenedFilesByProcess(pid)
	openedLogFiles := []string{}

	if err != nil {
		return nil, err
	}

	for _, filePath := range openedFiles {
		fileBaseName := filepath.Base(filePath)
		if matchLogPattern(fileBaseName) {
			last1000Text, err := getLastNBytes(filePath, 1000)
			if err != nil {
				continue
			}

			if IsMostlyASCII(last1000Text) {
				openedLogFiles = append(openedLogFiles, filePath)
			}
		}
	}

	return openedLogFiles, nil
}

// matchLogPattern checks if the filename matches any of the precompiled log patterns.
func matchLogPattern(s string) bool {
	for _, pattern := range logPatterns {
		if pattern.MatchString(s) {
			return true
		}
	}
	return false
}

// getLastNBytes opens the file at filename and returns up to the last n bytes. If the file
// is smaller than n bytes, the entire file contents are returned.
func getLastNBytes(filename string, n int64) ([]byte, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	// Get the file size
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()

	// Determine the offset
	offset := size - n
	if offset < 0 {
		offset = 0
	}

	// Seek to the offset
	_, err = file.Seek(offset, io.SeekStart)
	if err != nil {
		return nil, err
	}

	// Read the last n bytes (or less if the file is smaller)
	bytes := make([]byte, min(n, size))
	_, err = file.Read(bytes)
	if err != nil && err != io.EOF {
		return nil, err
	}

	return bytes, nil
}

// IsMostlyASCII determines if more than 70% of the bytes in b are ASCII.
// It returns true if the proportion of ASCII characters is greater than 0.7, and false otherwise.
func IsMostlyASCII(b []byte) bool {
	ASCIICount := 0

	for i := 0; i < len(b); i++ {
		if b[i] >= 32 && b[i] < unicode.MaxASCII {
			ASCIICount++
		}
	}

	return float64(ASCIICount)/float64(len(b)) > 0.7
}
