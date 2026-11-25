package capture

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jfrRecordingFixture reads a gzipped real recording from testdata/jfr:
//
//   - excluded-jdk8: Temurin 8u512 (format 2.0), started like yc's
//     -excludeSensitiveData (a base file, then the exclusions). One chunk.
//   - leaked-jdk21: Temurin 21.0.13 (format 2.1), started the same way while
//     another recording with the events on started and stopped inside its
//     window. Three chunks, and the events are in it.
//
// The base file enables jdk.JVMInformation, jdk.CPULoad and both sensitive
// events. The expected counts are what JDK 25's jfr tool prints.
func jfrRecordingFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "internal", "capture", "testdata", "jfr", name+".jfr.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestJFRCountEvents(t *testing.T) {
	names := append([]string{"jdk.JVMInformation", "jdk.CPULoad", "jdk.NotAnEvent"}, jfrSensitiveEvents...)
	tests := map[string]map[string]int{
		"excluded-jdk8": {"jdk.InitialEnvironmentVariable": 0, "jdk.InitialSystemProperty": 0, "jdk.JVMInformation": 1, "jdk.CPULoad": 2, "jdk.NotAnEvent": 0},
		"leaked-jdk21":  {"jdk.InitialEnvironmentVariable": 2, "jdk.InitialSystemProperty": 16, "jdk.JVMInformation": 3, "jdk.CPULoad": 5, "jdk.NotAnEvent": 0},
	}
	for name, want := range tests {
		data := jfrRecordingFixture(t, name)
		got, err := jfrCountEvents(bytes.NewReader(data), int64(len(data)), names)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: counts = %v, want %v", name, got, want)
		}
	}
}

func TestJFREventCountsFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "my.jfr")
	if err := os.WriteFile(p, jfrRecordingFixture(t, "leaked-jdk21"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := jfrEventCounts(p, jfrSensitiveEvents)
	if err != nil || got["jdk.InitialEnvironmentVariable"] != 2 || got["jdk.InitialSystemProperty"] != 16 {
		t.Errorf("jfrEventCounts() = %v, %v", got, err)
	}
	if _, err := jfrEventCounts(filepath.Join(t.TempDir(), "missing.jfr"), jfrSensitiveEvents); err == nil {
		t.Error("jfrEventCounts() of a missing file: no error")
	}
}

// A file yc can't fully read must be an error, never a count of zero.
func TestJFRCountEventsInvalid(t *testing.T) {
	good := jfrRecordingFixture(t, "leaked-jdk21")
	firstChunk := int(binary.BigEndian.Uint64(good[8:16]))
	edit := func(f func(b []byte) []byte) []byte { return f(bytes.Clone(good)) }

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "the file is empty"},
		{"not JFR", []byte(strings.Repeat("<configuration/>", 10)), "not a JFR file"},
		{"short header", good[:40], "incomplete chunk header"},
		{"version 1", edit(func(b []byte) []byte { b[5] = 1; return b }), "JFR file version 1.1"},
		{"truncated", good[:len(good)-100], "doesn't fit"},
		{"half a second chunk", good[:firstChunk+jfrChunkHeaderSize+10], "doesn't fit"},
		{"unfinished", edit(func(b []byte) []byte { b[64] = 255; return b }), "not finished"},
		{"uncompressed integers", edit(func(b []byte) []byte { b[67] &^= jfrFlagCompressed; return b }), "compressed integers"},
		{"metadata outside", edit(func(b []byte) []byte { binary.BigEndian.PutUint64(b[24:32], 0); return b }), "metadata position 0"},
		{"metadata not at metadata", edit(func(b []byte) []byte { binary.BigEndian.PutUint64(b[24:32], jfrChunkHeaderSize); return b }), "reading the metadata"},
		{"trailing garbage", append(bytes.Clone(good), "FLR\x00"...), "incomplete chunk header"},
	}
	for _, tt := range tests {
		_, err := jfrCountEvents(bytes.NewReader(tt.data), int64(len(tt.data)), jfrSensitiveEvents)
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: err = %v, want %q", tt.name, err, tt.want)
		}
	}
}

func TestJFRVarint(t *testing.T) {
	tests := []struct {
		in   []byte
		want int64
	}{
		{[]byte{0x00}, 0},
		{[]byte{0x7f}, 127},
		{[]byte{0x80, 0x01}, 128},
		// JFR pads some sizes to four bytes with empty continuation bytes.
		{[]byte{0x85, 0x80, 0x80, 0x00}, 5},
		// The ninth byte holds a full 8 bits.
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, -1},
	}
	for _, tt := range tests {
		in := &jfrInput{r: bufio.NewReader(bytes.NewReader(tt.in))}
		got, err := in.varint()
		if err != nil || got != tt.want || in.n != int64(len(tt.in)) {
			t.Errorf("varint(% x) = %d, %v after %d bytes, want %d after %d", tt.in, got, err, in.n, tt.want, len(tt.in))
		}
	}
	in := &jfrInput{r: bufio.NewReader(bytes.NewReader([]byte{0x80}))}
	if _, err := in.varint(); err == nil {
		t.Error("varint of a cut-off value: no error")
	}
}
