//go:build darwin
// +build darwin

package posix

// #cgo CFLAGS: -D__APPLE__=1
// #include <stdio.h>
// #include <stdlib.h>
//
// extern int jattach(int pid, int argc, char** argv);
import "C"
import "unsafe"

func Capture(pid int, args ...string) (ret int) {
	argv := make([]*C.char, len(args))
	for i, s := range args {
		cs := C.CString(s)
		defer C.free(unsafe.Pointer(cs))
		argv[i] = cs
	}
	ret = int(C.jattach(C.int(pid), C.int(len(args)), &argv[0]))
	return
}

func CaptureThreadDump(pid int) (ret int) {
	return Capture(pid, "threaddump")
}

func CaptureHeapDump(pid int, out string) (ret int) {
	return Capture(pid, "dumpheap", out)
}

func CaptureGCLog(pid int) (ret int) {
	return Capture(pid, "jcmd", "GC.class_stats")
}
