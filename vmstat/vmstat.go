//go:build linux
// +build linux

package vmstat

// #cgo CFLAGS: -DHAVE_CONFIG_H -include ./config.h -I./include -I./proc -D_GNU_SOURCE
/*
#include <stdio.h>
#include <stdlib.h>
#include <errno.h>

extern int vmstat(int argc, char** argv);
static void flush() {
	fflush(stderr);
	fflush(stdout);
}
*/
import "C"
import (
	"unsafe"
)

func VMStat(args ...string) (ret int) {
	argv := make([]*C.char, len(args))
	for i, s := range args {
		cs := C.CString(s)
		defer C.free(unsafe.Pointer(cs))
		argv[i] = cs
	}
	if len(args) == 0 {
		argv = []*C.char{nil}
		ret = int(C.vmstat(C.int(0), &argv[0]))
	} else {
		ret = int(C.vmstat(C.int(len(args)), &argv[0]))
	}
	C.flush()
	return
}
