//go:build linux || darwin
// +build linux darwin

package posix

/*
#include <stdio.h>
#include <stdlib.h>

extern void jattach1(int pid);
extern int jattach2(int pid, int argc, char** argv);
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

func CaptureGCLog(pid int) (ret int) {
	return Capture(pid, "jcmd", "GC.class_stats")
}
