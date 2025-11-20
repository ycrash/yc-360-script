//go:build !(linux || darwin || windows)

package capture

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	return []string{}, nil
}
