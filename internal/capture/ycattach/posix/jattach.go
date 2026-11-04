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
import "unsafe"

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

// CaptureGCLog captures GC statistics via jcmd. GC.class_stats was removed in
// JDK 15 (and needs -XX:+UnlockDiagnosticVMOptions on JDK 8), so fall back to
// GC.class_histogram, which every HotSpot and OpenJ9 JVM supports.
//
// OpenJ9 doesn't support GC.class_stats, and jattach reports success for every
// OpenJ9 jcmd even when the JVM rejects it, so the fallback can't trigger
// there. Go straight to GC.class_histogram on OpenJ9.
func CaptureGCLog(pid int) (ret int) {
	if C.jattach_is_openj9() != 0 {
		return Capture(pid, "jcmd", "GC.class_histogram")
	}
	ret = Capture(pid, "jcmd", "GC.class_stats")
	if ret == 0 {
		return
	}
	return Capture(pid, "jcmd", "GC.class_histogram")
}
