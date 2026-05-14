//go:build windows

package capture

import (
	"os"

	"golang.org/x/sys/windows"
)

func detectTargetArch(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)

	var targetWow64 bool
	if err = windows.IsWow64Process(h, &targetWow64); err != nil {
		return "", err
	}

	// On 32-bit Windows, everything is x86.
	if isOS64Bit() {
		if targetWow64 {
			return "x86", nil
		}
		return "x64", nil
	}
	return "x86", nil
}

func isOS64Bit() bool {
	return os.Getenv("PROCESSOR_ARCHITEW6432") != "" || os.Getenv("PROCESSOR_ARCHITECTURE") == "AMD64"
}
