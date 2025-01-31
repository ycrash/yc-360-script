package capture

import (
	"fmt"
	"os"
	"path/filepath"
)

// GetOpenedFilesByProcess returns a slice of file paths corresponding to all
// currently open file descriptors for the given PID. Internally, it inspects
// the /proc/<pid>/fd directory on Linux to resolve each file descriptor to its
// underlying file path via os.Readlink.
//
// Note: This function is specific to Linux environments that provide the
// /proc filesystem.
func GetOpenedFilesByProcess(pid int) ([]string, error) {
	dir := filepath.Join("/proc", fmt.Sprintf("%d", pid), "fd")
	openedFiles := []string{}

	if _, err := os.Stat(dir); err != nil {
		return openedFiles, err
	}

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		// If the file has vanished since we started, skip it
		if os.IsNotExist(err) {
			return nil
		}

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
