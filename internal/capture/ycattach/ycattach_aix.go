//go:build aix

package ycattach

// On AIX, jattach functionality is not supported.
// Return non-zero to indicate failure (same pattern as Linux error cases).

func Capture(pid int, args ...string) (ret int) {
    return -1
}

func CaptureThreadDump(pid int) (ret int) {
    return -1
}

func CaptureHeapDump(pid int, out string) (ret int) {
    return -1
}

func CaptureGCLog(pid int) (ret int) {
    return -1
}
