package capture

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestIsNodeGCLine(t *testing.T) {
	cases := []struct {
		line string
		want bool
	}{
		{"[30692:0x...]      113 ms: Scavenge 5.0 (5.1) -> 4.7 (6.1) MB", true},
		{"[30692:0x...]      200 ms: Mark-Compact 10.0 (12.0) -> 8.0 (11.0) MB", true},
		{"[30692:0x...]      200 ms: Mark-sweep 10.0 -> 8.0 MB", true},
		{"server listening on port 3000", false},
		{"", false},
		{"Scavenge without the colon-space token", false},
	}
	for _, c := range cases {
		if got := isNodeGCLine([]byte(c.line)); got != c.want {
			t.Errorf("isNodeGCLine(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestSplitGCLog(t *testing.T) {
	input := "server started\n" +
		"[1:0x1] 10 ms: Scavenge 1.0 -> 0.9 MB\n" +
		"handled request\n" +
		"[1:0x1] 20 ms: Mark-Compact 2.0 -> 1.5 MB\n" +
		"done\n"

	var gc, other bytes.Buffer
	gcLines, otherLines, err := SplitGCLog(bytes.NewReader([]byte(input)), &gc, &other)
	if err != nil {
		t.Fatalf("SplitGCLog error: %v", err)
	}
	if gcLines != 2 {
		t.Errorf("gcLines = %d, want 2", gcLines)
	}
	if otherLines != 3 {
		t.Errorf("otherLines = %d, want 3", otherLines)
	}
	if !bytes.Contains(gc.Bytes(), []byte("Scavenge")) || !bytes.Contains(gc.Bytes(), []byte("Mark-Compact")) {
		t.Errorf("gc output missing GC lines: %q", gc.String())
	}
	if bytes.Contains(gc.Bytes(), []byte("server started")) {
		t.Errorf("gc output leaked a non-GC line: %q", gc.String())
	}
	if !bytes.Contains(other.Bytes(), []byte("server started")) || !bytes.Contains(other.Bytes(), []byte("done")) {
		t.Errorf("other output missing app lines: %q", other.String())
	}
}

func TestSplitGCLogNoTrailingNewline(t *testing.T) {
	// A trailing line without a newline should still be classified.
	input := "app boot\n[1:0x1] 5 ms: Scavenge 1 -> 1 MB"
	var gc, other bytes.Buffer
	gcLines, otherLines, err := SplitGCLog(bytes.NewReader([]byte(input)), &gc, &other)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if gcLines != 1 || otherLines != 1 {
		t.Fatalf("gcLines=%d otherLines=%d, want 1/1", gcLines, otherLines)
	}
}

func TestReadNodeGCSliceIncremental(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")

	// Write an initial chunk with a complete line plus a partial (no newline) line.
	initial := "[1:0x1] 10 ms: Scavenge 1 -> 1 MB\napp log line\n[1:0x1] 20 ms: Mark-Comp"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	gc, other, off, err := ReadNodeGCSlice(path, 0)
	if err != nil {
		t.Fatalf("ReadNodeGCSlice: %v", err)
	}
	// The partial trailing line must NOT be included yet.
	if !bytes.Contains(gc, []byte("Scavenge")) {
		t.Errorf("expected Scavenge line, got gc=%q", gc)
	}
	if bytes.Contains(gc, []byte("Mark-Comp")) {
		t.Errorf("partial trailing line should not be returned: gc=%q", gc)
	}
	if !bytes.Contains(other, []byte("app log line")) {
		t.Errorf("expected app line, got other=%q", other)
	}

	// Complete the partial line and append another.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("act 2 -> 1 MB\nmore app\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	gc2, other2, off2, err := ReadNodeGCSlice(path, off)
	if err != nil {
		t.Fatalf("ReadNodeGCSlice(delta): %v", err)
	}
	if !bytes.Contains(gc2, []byte("Mark-Compact")) {
		t.Errorf("delta should contain the now-complete Mark-Compact line, got %q", gc2)
	}
	if !bytes.Contains(other2, []byte("more app")) {
		t.Errorf("delta should contain 'more app', got %q", other2)
	}
	if off2 <= off {
		t.Errorf("offset should advance: off=%d off2=%d", off, off2)
	}
}

func TestReadNodeGCSliceRotationDefense(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	if err := os.WriteFile(path, []byte("[1:0x1] 10 ms: Scavenge 1 -> 1 MB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pretend a previous cycle advanced far past the current (truncated) size.
	gc, _, newOff, err := ReadNodeGCSlice(path, 1_000_000)
	if err != nil {
		t.Fatalf("ReadNodeGCSlice: %v", err)
	}
	if !bytes.Contains(gc, []byte("Scavenge")) {
		t.Errorf("rotation defense should re-read from 0, got gc=%q", gc)
	}
	if newOff == 0 {
		t.Errorf("expected a non-zero advanced offset after re-read")
	}
}

func TestSplitNodeGCFromStreamsAndDiscards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stdout.log")
	content := "[1:0x1] 10 ms: Scavenge 1 -> 1 MB\napp line one\n[1:0x1] 20 ms: Mark-Compact 2 -> 1 MB\napp line two\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Streaming to two buffers.
	var gc, other bytes.Buffer
	off, gcLines, otherLines, err := SplitNodeGCFrom(path, 0, &gc, &other)
	if err != nil {
		t.Fatalf("SplitNodeGCFrom: %v", err)
	}
	if gcLines != 2 || otherLines != 2 {
		t.Errorf("gcLines=%d otherLines=%d, want 2/2", gcLines, otherLines)
	}
	if off != int64(len(content)) {
		t.Errorf("offset = %d, want %d", off, len(content))
	}
	if bytes.Contains(gc.Bytes(), []byte("app line")) {
		t.Errorf("gc leaked an app line: %q", gc.String())
	}

	// Discarding the non-GC side must not affect GC extraction or the offset.
	var gc2 bytes.Buffer
	off2, gcLines2, _, err := SplitNodeGCFrom(path, 0, &gc2, io.Discard)
	if err != nil {
		t.Fatalf("SplitNodeGCFrom(discard): %v", err)
	}
	if gcLines2 != 2 || off2 != off {
		t.Errorf("with discard: gcLines=%d off=%d, want 2 and %d", gcLines2, off2, off)
	}
	if gc2.String() != gc.String() {
		t.Errorf("discard changed GC output:\n%q\nvs\n%q", gc2.String(), gc.String())
	}
}

func TestNodeGCTracker(t *testing.T) {
	tr := NewNodeGCTracker()
	if got := tr.Offset(1, "/a"); got != 0 {
		t.Errorf("empty tracker offset = %d, want 0", got)
	}
	tr.SetOffset(1, "/a", 42)
	tr.SetOffset(2, "/b", 99)
	if got := tr.Offset(1, "/a"); got != 42 {
		t.Errorf("offset = %d, want 42", got)
	}

	tr.RetainOnly(map[int]string{2: "app"})
	if got := tr.Offset(1, "/a"); got != 0 {
		t.Errorf("pid 1 should have been dropped, got %d", got)
	}
	if got := tr.Offset(2, "/b"); got != 99 {
		t.Errorf("pid 2 should be retained, got %d", got)
	}

	tr.Forget(2)
	if got := tr.Offset(2, "/b"); got != 0 {
		t.Errorf("pid 2 should be forgotten, got %d", got)
	}
}

func TestNodeGCTrackerInitIfAbsent(t *testing.T) {
	tr := NewNodeGCTracker()

	// First sight: seeds the offset and reports true (initialized).
	if !tr.InitIfAbsent(7, "/std.log", 1000) {
		t.Fatalf("first InitIfAbsent should return true (initialized)")
	}
	if got := tr.Offset(7, "/std.log"); got != 1000 {
		t.Errorf("offset after init = %d, want 1000 (M3 first cycle must skip pre-existing history)", got)
	}

	// Second sight: must NOT overwrite, and reports false.
	if tr.InitIfAbsent(7, "/std.log", 2000) {
		t.Errorf("second InitIfAbsent should return false (already present)")
	}
	if got := tr.Offset(7, "/std.log"); got != 1000 {
		t.Errorf("offset must be unchanged by a no-op InitIfAbsent, got %d want 1000", got)
	}

	// A genuine stored offset of 0 counts as present, not a fresh sighting —
	// this is exactly the case a plain Offset()==0 check could not distinguish.
	tr.SetOffset(8, "/a.log", 0)
	if tr.InitIfAbsent(8, "/a.log", 500) {
		t.Errorf("InitIfAbsent must treat a stored 0 as present, not re-init")
	}
	if got := tr.Offset(8, "/a.log"); got != 0 {
		t.Errorf("stored 0 offset must be preserved, got %d", got)
	}
}
