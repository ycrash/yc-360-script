//go:build darwin || linux || aix

package posix

/*
#include <stdio.h>
#include <stdlib.h>

extern void jattach1(int pid);
extern int jattach2(int pid, int argc, char** argv);
extern int jattach_is_openj9(void);
extern int getenv_int(const char *name);

__attribute__((constructor)) void init() {
	int pid;
	pid = getenv_int("pid");
	if (pid <=0) {
		return;
	}
	jattach1(pid);
}

static void flush() {
	fflush(stderr);
	fflush(stdout);
}
*/
import "C"
import (
	"fmt"
	"unsafe"
)

func Capture(pid int, args ...string) (ret int) {
	argv := make([]*C.char, len(args))
	for i, s := range args {
		cs := C.CString(s)
		defer C.free(unsafe.Pointer(cs))
		argv[i] = cs
	}
	ret = int(C.jattach2(C.int(pid), C.int(len(args)), &argv[0]))
	C.flush()
	return
}

func CaptureThreadDump(pid int) (ret int) {
	return Capture(pid, "threaddump")
}

func CaptureHeapDump(pid int, out string) (ret int) {
	return Capture(pid, "dumpheap", out)
}

// CaptureGCLog captures GC statistics via jcmd GC.class_stats.
//
// There is deliberately no fallback when the JVM rejects it (JDK 15+ removed
// it): the other jcmd candidates either aren't GC data the server can parse
// (GC.class_histogram is a heap histogram) or force a GC in the target
// (GC.run, plain GC.class_histogram). The capture is reported as failed instead.
//
// OpenJ9 has no GC.class_stats, and jattach reports success for every OpenJ9
// jcmd even when the JVM rejects it, so the error text would be uploaded as
// GC data. Skip OpenJ9 with a non-zero exit instead.
func CaptureGCLog(pid int) (ret int) {
	if C.jattach_is_openj9() != 0 {
		fmt.Println("GC capture via jattach is not supported on OpenJ9")
		return 1
	}
	return Capture(pid, "jcmd", "GC.class_stats")
}
