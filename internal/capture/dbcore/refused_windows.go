//go:build windows

package dbcore

import "golang.org/x/sys/windows"

// refusedErrno is Winsock's "connection refused", which syscall.ECONNREFUSED does not match.
var refusedErrno error = windows.WSAECONNREFUSED
