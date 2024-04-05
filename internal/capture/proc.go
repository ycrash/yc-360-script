package capture

import (
	"os"
	"runtime"
	"syscall"
)

func IsProcessExists(pid int) (exists bool) {
	process, err := os.FindProcess(pid)
	if err == nil {
		if runtime.GOOS == "windows" {
			return true
		}
		err = process.Signal(syscall.Signal(0))
		if err == nil {
			return true
		}
		errno, ok := err.(syscall.Errno)
		if !ok {
			return false
		}
		switch errno {
		case syscall.ESRCH:
			return false
		case syscall.EPERM:
			return true
		}
	}
	return false
}
