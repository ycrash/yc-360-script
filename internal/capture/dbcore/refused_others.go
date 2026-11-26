//go:build !windows

package dbcore

import "syscall"

// refusedErrno is a dial's "connection refused".
var refusedErrno error = syscall.ECONNREFUSED
