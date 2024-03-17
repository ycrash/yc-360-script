//go:build !(linux || darwin || windows)
// +build !linux,!darwin,!windows

package main

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	return []string{}, nil
}
