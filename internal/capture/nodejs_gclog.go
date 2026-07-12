package capture

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"strings"
	"sync"

	psv3 "github.com/shirou/gopsutil/v3/process"
)

var (
	nodeGCScavengeToken = []byte(": Scavenge ")
	nodeGCMarkToken     = []byte(": Mark-")
)

func isNodeGCLine(line []byte) bool {
	return bytes.Contains(line, nodeGCScavengeToken) || bytes.Contains(line, nodeGCMarkToken)
}

// SplitGCLog reads newline-delimited content from src and routes each line to
// gcOut (V8 GC trace lines) or otherOut (everything else).
func SplitGCLog(src io.Reader, gcOut, otherOut io.Writer) (gcLines, otherLines int, err error) {
	r := bufio.NewReader(src)
	for {
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			if isNodeGCLine(line) {
				if _, werr := gcOut.Write(line); werr != nil {
					return gcLines, otherLines, werr
				}
				gcLines++
			} else {
				if _, werr := otherOut.Write(line); werr != nil {
					return gcLines, otherLines, werr
				}
				otherLines++
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return gcLines, otherLines, nil
			}
			return gcLines, otherLines, readErr
		}
	}
}

// NodeHasTraceGCFlag reports whether the target process was started with a V8
// GC-trace flag (--trace-gc / --trace_gc).
func NodeHasTraceGCFlag(pid int) bool {
	proc, err := psv3.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	cmdline, err := proc.Cmdline()
	if err != nil || cmdline == "" {
		return false
	}
	return strings.Contains(cmdline, "--trace-gc") || strings.Contains(cmdline, "--trace_gc")
}

// ResolveNodeStdoutFile resolves the target process's stdout (fd 1) to a
// regular file path, or returns an error if fd 1 is not a plain file (a pipe, a
// Docker/K8s log driver, or an unsupported platform).
func ResolveNodeStdoutFile(pid int) (string, error) {
	return resolveNodeStdoutFile(pid)
}

// SplitNodeGCFrom reads new content from path starting at startOffset and
// streams it, classified line by line, into gcOut (GC trace) and otherOut
// (everything else).
//
// newOffset is the offset to persist for the next incremental read (M3).
func SplitNodeGCFrom(path string, startOffset int64, gcOut, otherOut io.Writer) (newOffset int64, gcLines, otherLines int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return startOffset, 0, 0, err
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return startOffset, 0, 0, err
	}
	size := fi.Size()

	// Rotation/truncation defense.
	if startOffset > size {
		startOffset = 0
	}

	endOffset, err := findLastCompleteLineEnd(f, startOffset, size)
	if err != nil {
		return startOffset, 0, 0, err
	}
	if endOffset <= startOffset {
		// No new complete lines yet; keep the offset for next time.
		return startOffset, 0, 0, nil
	}

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return startOffset, 0, 0, err
	}

	gcLines, otherLines, err = SplitGCLog(io.LimitReader(f, endOffset-startOffset), gcOut, otherOut)
	if err != nil {
		return startOffset, gcLines, otherLines, err
	}
	return endOffset, gcLines, otherLines, nil
}

// ReadNodeGCSlice is a buffering convenience wrapper around SplitNodeGCFrom for
// callers (and tests) that want the split content in memory.
func ReadNodeGCSlice(path string, startOffset int64) (gcBytes, otherBytes []byte, newOffset int64, err error) {
	var gcBuf, otherBuf bytes.Buffer
	newOffset, _, _, err = SplitNodeGCFrom(path, startOffset, &gcBuf, &otherBuf)
	if err != nil {
		return nil, nil, newOffset, err
	}
	return gcBuf.Bytes(), otherBuf.Bytes(), newOffset, nil
}

// nodeGCKey identifies a persistent GC-tailing session by (pid, file).
type nodeGCKey struct {
	pid  int
	path string
}

// NodeGCTracker persists a byte offset per (pid, file) across M3 cycles so each
// cycle reads only the incremental delta of the continuously-growing GC/stdout
// file.
type NodeGCTracker struct {
	mu      sync.Mutex
	offsets map[nodeGCKey]int64
}

// NewNodeGCTracker creates an empty tracker.
func NewNodeGCTracker() *NodeGCTracker {
	return &NodeGCTracker{offsets: make(map[nodeGCKey]int64)}
}

// Offset returns the stored offset for (pid, path); 0 if none.
func (t *NodeGCTracker) Offset(pid int, path string) int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offsets[nodeGCKey{pid: pid, path: path}]
}

// SetOffset stores the offset for (pid, path).
func (t *NodeGCTracker) SetOffset(pid int, path string, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.offsets[nodeGCKey{pid: pid, path: path}] = offset
}

// InitIfAbsent records offset for (pid, path) only if nothing is stored yet,
// returning true when it performed that first-cycle initialization.
func (t *NodeGCTracker) InitIfAbsent(pid int, path string, offset int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := nodeGCKey{pid: pid, path: path}
	if _, ok := t.offsets[key]; ok {
		return false
	}
	t.offsets[key] = offset
	return true
}

// Forget drops all offset state for a pid (e.g. when it exits / is reconciled
// out), so state does not leak across process restarts and PID reuse.
func (t *NodeGCTracker) Forget(pid int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.offsets {
		if k.pid == pid {
			delete(t.offsets, k)
		}
	}
}

// RetainOnly drops offset state for any pid not present in keep.
func (t *NodeGCTracker) RetainOnly(keep map[int]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for k := range t.offsets {
		if _, ok := keep[k.pid]; !ok {
			delete(t.offsets, k)
		}
	}
}
