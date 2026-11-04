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

// CaptureGCLog captures GC statistics via jcmd GC.class_stats.
//
// There is deliberately no fallback when the JVM rejects it (JDK 15+ removed
// it): the other jcmd candidates either aren't GC data the server can parse
// (GC.class_histogram is a heap histogram) or force a GC in the target
// (GC.run, plain GC.class_histogram). The capture is reported as failed instead.
func CaptureGCLog(pid int) (ret int) {
	return capture(strconv.Itoa(pid), "jcmd", "GC.class_stats")
}
