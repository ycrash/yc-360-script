package capture

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// ── test writers ──────────────────────────────────────────────────────────────

type errWriter struct{}

func (errWriter) Write([]byte) (int, error) { return 0, errors.New("write error") }

type failAfterWriter struct{ limit, n int }

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if w.n >= w.limit {
		return 0, errors.New("write error")
	}
	allowed := w.limit - w.n
	if len(p) <= allowed {
		w.n += len(p)
		return len(p), nil
	}
	w.n = w.limit
	return allowed, errors.New("write error")
}

// ── test helpers ──────────────────────────────────────────────────────────────

// closedGCFile returns an *os.File whose descriptor has already been closed,
// so any OS call on it (Read, Seek, Stat) returns an error.
func closedGCFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "closed-*.json")
	if err != nil {
		t.Fatalf("closedGCFile: %v", err)
	}
	_ = f.Close()
	return f
}

func tempGCLog(t *testing.T, content string) *DotnetGCLog {
	t.Helper()
	f := tempGCFile(t, content)
	name := f.Name()
	_ = f.Close()
	l, err := OpenDotnetGCLog(name)
	if err != nil {
		t.Fatalf("tempGCLog: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// tempGCFile returns an open temp file positioned at the start; the caller closes it.
func tempGCFile(t *testing.T, content string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "gctest-*.json")
	if err != nil {
		t.Fatalf("tempGCFile: %v", err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("tempGCFile write: %v", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("tempGCFile seek: %v", err)
	}
	return f
}

func mustParseGCTime(t *testing.T, raw string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("mustParseGCTime %q: %v", raw, err)
	}
	return ts
}

// ── constants ─────────────────────────────────────────────────────────────────

const gcHeaderLine = `{"Platform":".Net"}` + "\n"
const gcEvent1 = `{"StopTime":"2026-03-12T21:17:15.1796239+05:30"}` + "\n"
const gcEvent2 = `{"StopTime":"2026-03-12T21:17:16.0000000+05:30"}` + "\n"

// ── readHeader ────────────────────────────────────────────────────────────────

func TestReadHeader(t *testing.T) {
	t.Run("returns header bytes and correct data offset", func(t *testing.T) {
		content := gcHeaderLine + gcEvent1 + gcEvent2
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		header, dataOffset, err := readHeader(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if header != gcHeaderLine {
			t.Errorf("header = %q, want %q", header, gcHeaderLine)
		}
		if dataOffset != int64(len(gcHeaderLine)) {
			t.Errorf("dataOffset = %d, want %d", dataOffset, len(gcHeaderLine))
		}
	})

	t.Run("empty file returns error", func(t *testing.T) {
		f := tempGCFile(t, "")
		defer f.Close() //nolint:errcheck

		_, _, err := readHeader(f)
		if err == nil {
			t.Fatal("expected error for empty file")
		}
	})

	t.Run("header only, no events, returns data offset == file size", func(t *testing.T) {
		f := tempGCFile(t, gcHeaderLine)
		defer f.Close() //nolint:errcheck

		_, dataOffset, err := readHeader(f)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if dataOffset != int64(len(gcHeaderLine)) {
			t.Errorf("dataOffset = %d, want %d", dataOffset, len(gcHeaderLine))
		}
	})

	t.Run("first line must be .Net header", func(t *testing.T) {
		f := tempGCFile(t, gcEvent1+gcEvent2)
		defer f.Close() //nolint:errcheck

		_, _, err := readHeader(f)
		if err == nil {
			t.Fatal("expected error for missing .Net header")
		}
	})
}

func TestReadHeaderErrors(t *testing.T) {
	t.Run("seek error returns error", func(t *testing.T) {
		_, _, err := readHeader(closedGCFile(t))
		if err == nil {
			t.Fatal("expected error from closed file")
		}
	})
}

// ── readLineAtOrAfter ─────────────────────────────────────────────────────────

func TestReadLineAtOrAfter(t *testing.T) {
	content := gcHeaderLine + gcEvent1 + gcEvent2
	off0 := int64(0)
	off1 := int64(len(gcHeaderLine))
	off2 := int64(len(gcHeaderLine) + len(gcEvent1))
	offEOF := int64(len(content))

	t.Run("offset at line boundary returns that line", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		lineStartOffset, line, err := readLineAtOrAfter(f, off1, offEOF)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lineStartOffset != off1 {
			t.Errorf("lineStartOffset = %d, want %d", lineStartOffset, off1)
		}
		if string(line) != gcEvent1 {
			t.Errorf("line = %q, want %q", line, gcEvent1)
		}
		if next := lineStartOffset + int64(len(line)); next != off2 {
			t.Errorf("next = %d, want %d", next, off2)
		}
	})

	t.Run("offset mid-line advances to next line", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		mid := off1 + 5 // somewhere inside gcEvent1
		lineStartOffset, line, err := readLineAtOrAfter(f, mid, offEOF)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lineStartOffset != off2 {
			t.Errorf("lineStartOffset = %d, want %d", lineStartOffset, off2)
		}
		if string(line) != gcEvent2 {
			t.Errorf("line = %q, want %q", line, gcEvent2)
		}
		if next := lineStartOffset + int64(len(line)); next != offEOF {
			t.Errorf("next = %d, want %d", next, offEOF)
		}
	})

	t.Run("offset 0 returns header line", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		lineStartOffset, line, err := readLineAtOrAfter(f, off0, offEOF)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if lineStartOffset != 0 {
			t.Errorf("lineStartOffset = %d, want 0", lineStartOffset)
		}
		if string(line) != gcHeaderLine {
			t.Errorf("line = %q, want %q", line, gcHeaderLine)
		}
		if next := lineStartOffset + int64(len(line)); next != off1 {
			t.Errorf("next = %d, want %d", next, off1)
		}
	})

	t.Run("offset at EOF returns io.EOF", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		_, _, err := readLineAtOrAfter(f, offEOF, offEOF)
		if err != io.EOF {
			t.Errorf("expected io.EOF, got %v", err)
		}
	})
}

func TestReadLineAtOrAfterEdgeCases(t *testing.T) {
	t.Run("last line without trailing newline treated as incomplete", func(t *testing.T) {
		content := gcHeaderLine + `{"StopTime":"2026-03-12T21:17:15.1+05:30"}` // no newline
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		off := int64(len(gcHeaderLine))
		fileSize := int64(len(content))
		_, _, err := readLineAtOrAfter(f, off, fileSize)
		if err != io.EOF {
			t.Errorf("expected io.EOF for unterminated line, got %v", err)
		}
	})

	t.Run("mid-line probe on last line without newline returns EOF", func(t *testing.T) {
		content := gcHeaderLine + `{"StopTime":"2026-03-12T21:17:15.1+05:30"}` // no newline
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		mid := int64(len(gcHeaderLine)) + 5 // inside last line
		fileSize := int64(len(content))
		_, _, err := readLineAtOrAfter(f, mid, fileSize)
		if err != io.EOF {
			t.Errorf("expected io.EOF, got %v", err)
		}
	})

	t.Run("seek error on open returns error", func(t *testing.T) {
		_, _, err := readLineAtOrAfter(closedGCFile(t), 0, 100)
		if err == nil {
			t.Fatal("expected error from closed file")
		}
	})
}

// ── findOffsetAfter ────────────────────────────────────────────────────────────

func TestFindOffsetAfter(t *testing.T) {
	events := []string{
		`{"StopTime":"2026-03-12T21:17:15.1000000+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:17:15.2000000+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:17:15.3000000+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:17:15.4000000+05:30"}` + "\n",
	}
	content := gcHeaderLine + strings.Join(events, "")
	dataOff := int64(len(gcHeaderLine))
	off := []int64{
		dataOff,
		dataOff + int64(len(events[0])),
		dataOff + int64(len(events[0])) + int64(len(events[1])),
		dataOff + int64(len(events[0])) + int64(len(events[1])) + int64(len(events[2])),
	}
	fileSize := int64(len(content))

	tests := []struct {
		name       string
		after      string
		wantOffset int64
	}{
		{
			name:       "before all events → first event",
			after:      "2026-03-12T21:17:14+05:30",
			wantOffset: off[0],
		},
		{
			name:       "exactly first event StopTime → first event",
			after:      "2026-03-12T21:17:15.1000000+05:30",
			wantOffset: off[0],
		},
		{
			name:       "between event 1 and 2 → event 2",
			after:      "2026-03-12T21:17:15.1500000+05:30",
			wantOffset: off[1],
		},
		{
			name:       "exactly last event StopTime → last event",
			after:      "2026-03-12T21:17:15.4000000+05:30",
			wantOffset: off[3],
		},
		{
			name:       "after all events → file size (no match)",
			after:      "2026-03-12T21:17:16+05:30",
			wantOffset: fileSize,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := tempGCLog(t, content)
			_, got, err := l.findOffsetAfter(mustParseGCTime(t, tc.after))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantOffset {
				t.Errorf("got offset %d, want %d", got, tc.wantOffset)
			}
		})
	}

	t.Run("line missing StopTime returns error", func(t *testing.T) {
		l := tempGCLog(t, gcHeaderLine+`{"Other":"value"}`+"\n")
		_, _, err := l.findOffsetAfter(mustParseGCTime(t, "2026-03-12T21:17:14+05:30"))
		if err == nil {
			t.Fatal("expected error for missing StopTime")
		}
	})

	t.Run("stat error returns error", func(t *testing.T) {
		l := &DotnetGCLog{file: closedGCFile(t), eventsOffset: 0}
		_, _, err := l.findOffsetAfter(time.Time{})
		if err == nil {
			t.Fatal("expected error from closed file")
		}
	})
}

// ── copyFromOffset ────────────────────────────────────────────────────────────

func TestCopyFromOffset(t *testing.T) {
	content := gcHeaderLine + gcEvent1 + gcEvent2
	contentSize := int64(len(content))

	t.Run("copies from start of events", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		var buf bytes.Buffer
		if err := copyFromOffset(&buf, f, int64(len(gcHeaderLine)), contentSize); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := gcEvent1 + gcEvent2
		if buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("copies from mid-file offset", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		var buf bytes.Buffer
		off := int64(len(gcHeaderLine) + len(gcEvent1))
		if err := copyFromOffset(&buf, f, off, contentSize); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.String() != gcEvent2 {
			t.Errorf("got %q, want %q", buf.String(), gcEvent2)
		}
	})

	t.Run("copies nothing when offset is at EOF", func(t *testing.T) {
		f := tempGCFile(t, content)
		defer f.Close() //nolint:errcheck

		var buf bytes.Buffer
		if err := copyFromOffset(&buf, f, contentSize, contentSize); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("expected empty output, got %q", buf.String())
		}
	})

	t.Run("skips unterminated trailing event during bulk copy", func(t *testing.T) {
		contentWithPartialTail := gcHeaderLine + gcEvent1 + `{"StopTime":"2026-03-12T21:17:17.0000000+05:30"}`
		f := tempGCFile(t, contentWithPartialTail)
		defer f.Close() //nolint:errcheck

		var buf bytes.Buffer
		if err := copyFromOffset(&buf, f, int64(len(gcHeaderLine)), int64(len(contentWithPartialTail))); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.String() != gcEvent1 {
			t.Errorf("got %q, want %q", buf.String(), gcEvent1)
		}
	})

	t.Run("copies nothing when only remaining bytes are an incomplete line", func(t *testing.T) {
		contentWithPartialTail := gcHeaderLine + gcEvent1 + `{"StopTime":"2026-03-12T21:17:17.0000000+05:30"}`
		f := tempGCFile(t, contentWithPartialTail)
		defer f.Close() //nolint:errcheck

		var buf bytes.Buffer
		startOffset := int64(len(gcHeaderLine) + len(gcEvent1))
		if err := copyFromOffset(&buf, f, startOffset, int64(len(contentWithPartialTail))); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("expected empty output, got %q", buf.String())
		}
	})

	t.Run("seek error returns error", func(t *testing.T) {
		err := copyFromOffset(&bytes.Buffer{}, closedGCFile(t), 0, 100)
		if err == nil {
			t.Fatal("expected error from closed file")
		}
	})
}

// ── DotnetGCLog (OpenDotnetGCLog / CopyAfter / CopyLast / Header / Close) ───

func TestDotnetGCLogOpen(t *testing.T) {
	t.Run("opens valid file", func(t *testing.T) {
		content := gcHeaderLine + gcEvent1 + gcEvent2
		f := tempGCFile(t, content)
		path := f.Name()
		_ = f.Close()

		jf, err := OpenDotnetGCLog(path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer jf.Close() //nolint:errcheck

		if jf.Header() != gcHeaderLine {
			t.Errorf("Header() = %q, want %q", jf.Header(), gcHeaderLine)
		}
	})

	t.Run("non-existent path returns error", func(t *testing.T) {
		_, err := OpenDotnetGCLog("/no/such/file.json")
		if err == nil {
			t.Fatal("expected error for missing file")
		}
	})

	t.Run("invalid header returns error", func(t *testing.T) {
		f := tempGCFile(t, gcEvent1+gcEvent2)
		path := f.Name()
		_ = f.Close()

		_, err := OpenDotnetGCLog(path)
		if err == nil {
			t.Fatal("expected error for missing .Net header")
		}
	})
}

func TestDotnetGCLogCopyAfter(t *testing.T) {
	events := []string{
		`{"StopTime":"2026-03-12T21:17:15.1000000+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:17:15.2000000+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:17:15.3000000+05:30"}` + "\n",
	}
	content := gcHeaderLine + strings.Join(events, "")

	t.Run("returns header + matching suffix", func(t *testing.T) {
		jf := tempGCLog(t, content)
		var buf bytes.Buffer
		if err := jf.CopyAfter(&buf, mustParseGCTime(t, "2026-03-12T21:17:15.2000000+05:30")); err != nil {
			t.Fatalf("CopyAfter: %v", err)
		}
		want := gcHeaderLine + events[1] + events[2]
		if buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("after before all events outputs header and all events", func(t *testing.T) {
		jf := tempGCLog(t, content)
		var buf bytes.Buffer
		if err := jf.CopyAfter(&buf, mustParseGCTime(t, "2026-03-12T21:17:14+05:30")); err != nil {
			t.Fatalf("CopyAfter: %v", err)
		}
		if buf.String() != content {
			t.Errorf("got %q, want %q", buf.String(), content)
		}
	})

	t.Run("no matching events outputs header only", func(t *testing.T) {
		jf := tempGCLog(t, content)
		var buf bytes.Buffer
		if err := jf.CopyAfter(&buf, mustParseGCTime(t, "2026-03-12T21:17:16+05:30")); err != nil {
			t.Fatalf("CopyAfter: %v", err)
		}
		if buf.String() != gcHeaderLine {
			t.Errorf("got %q, want %q", buf.String(), gcHeaderLine)
		}
	})

	t.Run("excludes unterminated trailing event from output", func(t *testing.T) {
		contentWithPartialTail := gcHeaderLine + events[0] + `{"StopTime":"2026-03-12T21:17:15.4000000+05:30"}`
		jf := tempGCLog(t, contentWithPartialTail)
		var buf bytes.Buffer
		if err := jf.CopyAfter(&buf, mustParseGCTime(t, "2026-03-12T21:17:15.0000000+05:30")); err != nil {
			t.Fatalf("CopyAfter: %v", err)
		}
		want := gcHeaderLine + events[0]
		if buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("write error propagates", func(t *testing.T) {
		jf := tempGCLog(t, content)
		err := jf.CopyAfter(errWriter{}, mustParseGCTime(t, "2026-03-12T21:17:14+05:30"))
		if err == nil {
			t.Fatal("expected write error")
		}
	})

	t.Run("partial write error propagates", func(t *testing.T) {
		jf := tempGCLog(t, content)
		// Allow exactly the header through, then fail on the first event line.
		w := &failAfterWriter{limit: len(gcHeaderLine)}
		err := jf.CopyAfter(w, mustParseGCTime(t, "2026-03-12T21:17:14+05:30"))
		if err == nil {
			t.Fatal("expected write error on event copy")
		}
	})
}

func TestDotnetGCLogCopyLast(t *testing.T) {
	events := []string{
		`{"StopTime":"2026-03-12T20:55:00+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:10:00+05:30"}` + "\n",
		`{"StopTime":"2026-03-12T21:25:00+05:30"}` + "\n",
	}
	content := gcHeaderLine + strings.Join(events, "")

	t.Run("copies the requested recent window", func(t *testing.T) {
		jf := tempGCLog(t, content)
		var buf bytes.Buffer
		now := mustParseGCTime(t, "2026-03-12T21:30:00+05:30")
		if err := jf.CopyLast(&buf, now, 30*time.Minute); err != nil {
			t.Fatalf("CopyLast: %v", err)
		}
		want := gcHeaderLine + events[1] + events[2]
		if buf.String() != want {
			t.Errorf("got %q, want %q", buf.String(), want)
		}
	})

	t.Run("negative window returns error", func(t *testing.T) {
		jf := tempGCLog(t, content)
		err := jf.CopyLast(&bytes.Buffer{}, mustParseGCTime(t, "2026-03-12T21:30:00+05:30"), -time.Minute)
		if err == nil {
			t.Fatal("expected error for negative window")
		}
	})
}
