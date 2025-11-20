//go:build windows

package windows

// #include <stdio.h>
// #include <stdlib.h>
//
// extern int jattach(int argc, char** argv);
import "C"
import (
	"strconv"
	"unsafe"
)

func Capture(pid int, args ...string) (ret int) {
	a := make([]string, len(args)+1)
	a[0] = strconv.Itoa(pid)
	copy(a[1:], args)
	return capture(a...)
}

func capture(args ...string) (ret int) {
	argv := make([]*C.char, len(args))
	for i, s := range args {
		cs := C.CString(s)
		defer C.free(unsafe.Pointer(cs))
		argv[i] = cs
	}
	ret = int(C.jattach(C.int(len(args)), &argv[0]))
	C.fflush(C.stdout)
	C.fflush(C.stderr)
	return
}

func CaptureThreadDump(pid int) (ret int) {
	return capture(strconv.Itoa(pid), "threaddump")
}

func CaptureHeapDump(pid int, out string) (ret int) {
	return capture(strconv.Itoa(pid), "dumpheap", out)
}

func CaptureGCLog(pid int) (ret int) {
	return capture(strconv.Itoa(pid), "jcmd", "GC.class_stats")
}
