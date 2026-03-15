package capture

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tidwall/gjson"
)

// DotnetGCLog provides streaming, time-filtered access to a .NET GC log
// JSONL file. Create one with [OpenDotnetGCLog] and call [DotnetGCLog.Close]
// when done.
type DotnetGCLog struct {
	file         *os.File
	header       string
	eventsOffset int64
}

// OpenDotnetGCLog opens path, validates the .NET GC log header line, and
// returns a DotnetGCLog. The caller must call Close when done.
func OpenDotnetGCLog(path string) (*DotnetGCLog, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	header, eventsOffset, err := readHeader(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	return &DotnetGCLog{file: file, header: header, eventsOffset: eventsOffset}, nil
}

// Close closes the underlying file.
func (d *DotnetGCLog) Close() error { return d.file.Close() }

// Header returns the validated header line, including the trailing newline.
func (d *DotnetGCLog) Header() string { return d.header }

// CopyLast writes the last window of events ending at now.
//
// This is the most common convenience API for callers that want recent GC
// activity such as "the last 30 minutes".
func (d *DotnetGCLog) CopyLast(w io.Writer, now time.Time, window time.Duration) error {
	if window < 0 {
		return fmt.Errorf("window must be non-negative")
	}
	return d.CopyAfter(w, now.Add(-window))
}

// CopyAfter writes the header line followed by all event lines whose StopTime
// is >= after to w. Events are located via binary search so only the matching
// suffix is streamed.
func (d *DotnetGCLog) CopyAfter(w io.Writer, after time.Time) error {
	fileSize, startOffset, err := d.findOffsetAfter(after)
	if err != nil {
		return err
	}
	if _, err := w.Write([]byte(d.header)); err != nil {
		return err
	}
	return copyFromOffset(w, d.file, startOffset, fileSize)
}

// findOffsetAfter returns the file size at call time and the byte offset of
// the first event line whose StopTime is >= after. Both the binary search and
// the caller's subsequent copy are bounded to this file size so that bytes
// appended by a concurrent writer are ignored. Returns eventsOffset if all
// events qualify, or fileSize if none do.
func (d *DotnetGCLog) findOffsetAfter(after time.Time) (fileSize, startOffset int64, err error) {
	fi, err := d.file.Stat()
	if err != nil {
		return 0, 0, err
	}
	fileSize = fi.Size()

	lo, hi := d.eventsOffset, fileSize
	for lo < hi {
		// binary search
		mid := (lo + hi) / 2
		lineStartOffset, line, lineErr := readLineAtOrAfter(d.file, mid, fileSize)
		if lineErr == io.EOF {
			hi = mid
			continue
		}
		if lineErr != nil {
			return 0, 0, lineErr
		}
		lineTime, parseErr := parseStopTime(line, lineStartOffset)
		if parseErr != nil {
			return 0, 0, parseErr
		}
		if !lineTime.Before(after) {
			hi = mid // must use mid, not lineStartOffset — ensures hi strictly decreases
		} else {
			lo = lineStartOffset + int64(len(line))
		}
	}
	return fileSize, lo, nil
}

func readHeader(file *os.File) (header string, eventsOffset int64, err error) {
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(file)
	line, readErr := r.ReadBytes('\n')
	if readErr != nil && readErr != io.EOF {
		err = readErr
		return
	}
	if len(line) == 0 {
		err = fmt.Errorf("file is empty")
		return
	}
	platform := gjson.GetBytes(line, "Platform")
	if !platform.Exists() || platform.Str != ".Net" {
		err = fmt.Errorf("line 1 is not the expected .NET GC log header")
		return
	}
	header = string(line)
	eventsOffset = int64(len(line))
	return
}

// readLineAtOrAfter reads the first complete newline-terminated line at or
// after the given byte offset, bounded by fileSize to avoid reading
// partially-written trailing lines. Returns the line's starting offset and
// its content (including the trailing newline).
//
// Given a GC log file (fileSize=N):
//
//	offset 0:           {"Platform":".Net"}\n
//	offset 20:          {"Generation":0,...,"StopTime":"...15.17..."}\n
//	offset 500:         {"Generation":1,...,"StopTime":"...15.35..."}\n
//
//	offset=0   → lineStartOffset=0,   line=`{"Platform":".Net"}\n`    (at line boundary)
//	offset=10  → lineStartOffset=20,  line=`{"Generation":0,...}\n`   (mid-line, advances to next)
//	offset=20  → lineStartOffset=20,  line=`{"Generation":0,...}\n`   (at line boundary)
//	offset=500 → lineStartOffset=500, line=`{"Generation":1,...}\n`   (at line boundary)
//	offset=N   → io.EOF                                              (at/past fileSize)
//
// If the last line lacks a trailing newline it is treated as incomplete
// (partially written by a concurrent writer) and returns io.EOF.
func readLineAtOrAfter(f *os.File, offset, fileSize int64) (lineStartOffset int64, line []byte, err error) {
	if offset >= fileSize {
		err = io.EOF
		return
	}

	// Seek one byte before offset to check if we're at a line boundary.
	seekTo := offset
	if offset > 0 {
		seekTo = offset - 1
	}
	if _, err = f.Seek(seekTo, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(io.LimitReader(f, fileSize-seekTo))

	if offset > 0 {
		// Read through end of the line containing the byte at offset-1.
		prefix, readErr := r.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			err = readErr
			return
		}
		if prefix[0] == '\n' {
			// Byte at offset-1 was '\n': offset is already a line boundary.
			lineStartOffset = offset
		} else {
			// Mid-line: advance past the remainder.
			lineStartOffset = seekTo + int64(len(prefix))
			if readErr == io.EOF {
				err = io.EOF
				return
			}
		}
	}

	// r is now positioned at lineStartOffset.
	lineBytes, readErr := r.ReadBytes('\n')
	if readErr == io.EOF {
		// Either no data at all, or a non-newline-terminated trailing fragment.
		// In a concurrently-written file the latter is a partially-written line;
		// treat both cases as "no more complete lines".
		err = io.EOF
		return
	} else if readErr != nil {
		err = readErr
		return
	}
	line = lineBytes
	return
}

func parseStopTime(line []byte, offset int64) (time.Time, error) {
	ts := gjson.GetBytes(line, "StopTime")
	if !ts.Exists() {
		return time.Time{}, fmt.Errorf("line at offset %d missing StopTime field", offset)
	}
	lineTime, err := time.Parse(time.RFC3339Nano, ts.Str)
	if err != nil {
		return time.Time{}, fmt.Errorf("line at offset %d has unparseable StopTime %q: %v", offset, ts.Str, err)
	}
	return lineTime, nil
}

// copyFromOffset copies all bytes from startOffset up to fileSize. The fileSize
// bound prevents reading partially-written lines appended by a concurrent writer.
func copyFromOffset(w io.Writer, f *os.File, startOffset, fileSize int64) error {
	endOffset, err := findLastCompleteLineEnd(f, startOffset, fileSize)
	if err != nil {
		return err
	}
	if endOffset <= startOffset {
		return nil
	}
	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return err
	}
	_, err = io.CopyN(w, f, endOffset-startOffset)
	return err
}

// findLastCompleteLineEnd returns the byte offset immediately after the last
// newline in [startOffset, fileSize). If no newline exists in that range, there
// is no complete line to copy and startOffset is returned.
func findLastCompleteLineEnd(f *os.File, startOffset, fileSize int64) (int64, error) {
	if startOffset >= fileSize {
		return startOffset, nil
	}

	const chunkSize int64 = 64 * 1024
	buf := make([]byte, chunkSize)

	for chunkEnd := fileSize; chunkEnd > startOffset; {
		chunkStart := chunkEnd - chunkSize
		if chunkStart < startOffset {
			chunkStart = startOffset
		}

		size := int(chunkEnd - chunkStart)
		n, err := f.ReadAt(buf[:size], chunkStart)
		if err != nil && err != io.EOF {
			return 0, err
		}
		if idx := bytes.LastIndexByte(buf[:n], '\n'); idx >= 0 {
			return chunkStart + int64(idx) + 1, nil
		}

		chunkEnd = chunkStart
	}

	return startOffset, nil
}
