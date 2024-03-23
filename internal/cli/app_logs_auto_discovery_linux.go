package cli

import (
	"fmt"
	"os"
	"path/filepath"
)

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
