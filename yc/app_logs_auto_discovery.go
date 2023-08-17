package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"shell"
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
			last1000Text, err := lastNText(filePath, 1000)
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

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	dir := filepath.Join("/proc", fmt.Sprintf("%d", pid), "fd")
	openedFiles := []string{}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if !info.IsDir() {
			filePath := filepath.Join(dir, info.Name())

			// Resolve symlink
			dst, err := os.Readlink(filePath)

			if err != nil {
				return err
			}

			openedFiles = append(openedFiles, dst)
		}

		return nil
	})

	return openedFiles, err
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

func lastNText(file string, N uint) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}

	err = shell.PositionLastLines(f, N)
	if err != nil {
		return nil, err
	}

	return io.ReadAll(f)
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
