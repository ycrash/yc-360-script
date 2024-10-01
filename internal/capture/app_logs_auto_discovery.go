package capture

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"unicode"
)

func DiscoverOpenedLogFilesByProcess(pid int) ([]string, error) {
	if runtime.GOOS != "linux" {
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

			if IsHumanReadable(last1000Text) {
				openedLogFiles = append(openedLogFiles, filePath)
			}
		}
	}

	return openedLogFiles, err
}

func matchLogPattern(s string) bool {
	patterns := []string{
		".*\\.log",     // *.log
		".*log.*\\..*", // *log*.*
	}

	match := false

	// if matches one of the pattern above, return true
	for _, pattern := range patterns {
		m, _ := regexp.MatchString(pattern, s)
		if m {
			return true
		}
	}

	return match
}

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

func IsHumanReadable(b []byte) bool {
	ASCIICount := 0

	for i := 0; i < len(b); i++ {
		if b[i] >= 32 && b[i] < unicode.MaxASCII {
			ASCIICount++
		}
	}

	return float64(ASCIICount)/float64(len(b)) > 0.7
}
