//go:build windows

package capture

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Go's standard net package has no support for Windows named pipes. Rather than
// add the github.com/Microsoft/go-winio dependency, we open the pipe directly
// with the Win32 CreateFile API (via golang.org/x/sys/windows, already a
// dependency here) and wrap the handle as a net.Conn. The pipe is opened with
// FILE_FLAG_OVERLAPPED so os.NewFile associates it with Go's runtime poller,
// which makes SetDeadline work for the asynchronous dumpGC/dumpCPUProfile RPCs.
//
// Security note: on Windows the named pipe itself is NOT permission-protected
// (see the requirements' §11.1). The shared-secret token carried in every
// request is the real access control here, not the pipe's own ACL.

// ERROR_PIPE_BUSY (231): all pipe instances are busy; wait and retry.
const errorPipeBusy = syscall.Errno(231)

var (
	nodeModKernel32       = windows.NewLazySystemDLL("kernel32.dll")
	nodeProcWaitNamedPipe = nodeModKernel32.NewProc("WaitNamedPipeW")
)

type winPipeAddr struct{ path string }

func (a winPipeAddr) Network() string { return "pipe" }
func (a winPipeAddr) String() string  { return a.path }

// winPipeConn adapts an *os.File over a named-pipe handle to net.Conn. os.File
// already provides Read/Write/Close/SetDeadline; only the addr accessors are
// missing from the net.Conn interface.
type winPipeConn struct {
	*os.File
	addr winPipeAddr
}

func (c *winPipeConn) LocalAddr() net.Addr  { return c.addr }
func (c *winPipeConn) RemoteAddr() net.Addr { return c.addr }

func dialNodePipe(pipePath string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	pathPtr, err := windows.UTF16PtrFromString(pipePath)
	if err != nil {
		return nil, err
	}

	for {
		handle, err := windows.CreateFile(
			pathPtr,
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			0,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OVERLAPPED,
			0,
		)
		if err == nil {
			f := os.NewFile(uintptr(handle), pipePath)
			if f == nil {
				_ = windows.CloseHandle(handle)
				return nil, fmt.Errorf("failed wrapping node pipe handle for %s", pipePath)
			}
			return &winPipeConn{File: f, addr: winPipeAddr{path: pipePath}}, nil
		}

		if errno, ok := err.(syscall.Errno); ok && errno == errorPipeBusy {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, fmt.Errorf("timed out waiting for busy node pipe %s", pipePath)
			}
			waitMs := uint32(remaining / time.Millisecond)
			if waitMs == 0 {
				waitMs = 1
			}
			if werr := waitNamedPipe(pathPtr, waitMs); werr != nil {
				return nil, fmt.Errorf("WaitNamedPipe %s failed: %w", pipePath, werr)
			}
			continue
		}

		return nil, fmt.Errorf("CreateFile %s failed: %w", pipePath, err)
	}
}

func waitNamedPipe(name *uint16, timeoutMs uint32) error {
	r1, _, e1 := nodeProcWaitNamedPipe.Call(uintptr(unsafe.Pointer(name)), uintptr(timeoutMs))
	if r1 == 0 {
		if e1 != syscall.Errno(0) {
			return e1
		}
		return fmt.Errorf("WaitNamedPipe failed")
	}
	return nil
}
