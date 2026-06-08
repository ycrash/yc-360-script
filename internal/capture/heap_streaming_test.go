package capture

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yc-agent/internal/config"

	"github.com/klauspost/compress/zstd"
)

// TestUploadCapturedFileStreamsZstd verifies that UploadCapturedFile sends the
// heap dump zstd-compressed on the fly
func TestUploadCapturedFileStreamsZstd(t *testing.T) {
	prevOnlyCapture := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.OnlyCapture = false
	defer func() { config.GlobalConfig.OnlyCapture = prevOnlyCapture }()

	// Compressible-but-non-trivial payload, larger than the pipe/zstd buffers so
	// the streaming path actually exercises multiple read/write cycles.
	raw := bytes.Repeat([]byte("heap dump payload \x00\x01\x02\xfe\xff "), 200_000)

	var (
		gotEncoding string
		gotBody     []byte
		decodeErr   error
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEncoding = r.URL.Query().Get("Content-Encoding")

		dec, err := zstd.NewReader(r.Body)
		if err != nil {
			decodeErr = err
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer dec.Close()

		gotBody, decodeErr = io.ReadAll(dec)
		if decodeErr != nil {
			http.Error(w, decodeErr.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// The raw heap dump file that UploadCapturedFile streams from.
	f, err := os.CreateTemp(t.TempDir(), "heap_dump.out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(raw); err != nil {
		t.Fatal(err)
	}

	hd := NewHeapDump("", 0, "", false)
	// Endpoint already carries a query string; PostData appends "&dt=...".
	hd.SetEndpoint(srv.URL + "?k=test")

	res := hd.UploadCapturedFile(f)

	if !res.Ok {
		t.Fatalf("upload not ok: %s", res.Msg)
	}
	if decodeErr != nil {
		t.Fatalf("server failed to zstd-decode body: %v", decodeErr)
	}
	if gotEncoding != "zst" {
		t.Errorf("Content-Encoding = %q, want %q", gotEncoding, "zst")
	}
	if !bytes.Equal(gotBody, raw) {
		t.Errorf("decoded body mismatch: got %d bytes, want %d", len(gotBody), len(raw))
	}
}

// TestUploadCapturedFileSkipsInOnlyCapture verifies no upload is attempted in
// only-capture mode (and therefore no compression work is done).
func TestUploadCapturedFileSkipsInOnlyCapture(t *testing.T) {
	prevOnlyCapture := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.OnlyCapture = true
	defer func() { config.GlobalConfig.OnlyCapture = prevOnlyCapture }()

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f, err := os.CreateTemp(t.TempDir(), "heap_dump.out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}

	hd := NewHeapDump("", 0, "", false)
	hd.SetEndpoint(srv.URL + "?k=test")

	res := hd.UploadCapturedFile(f)

	if res.Ok {
		t.Errorf("expected not-ok result in only-capture mode, got ok: %s", res.Msg)
	}
	if hit {
		t.Errorf("server was contacted in only-capture mode; expected no upload")
	}
}

func TestUploadCapturedFileSkipsEmptyFile(t *testing.T) {
	prevOnlyCapture := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.OnlyCapture = false
	defer func() { config.GlobalConfig.OnlyCapture = prevOnlyCapture }()

	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f, err := os.CreateTemp(t.TempDir(), "heap_dump.out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	hd := NewHeapDump("", 0, "", false)
	hd.SetEndpoint(srv.URL + "?k=test")

	res := hd.UploadCapturedFile(f)

	if res.Ok {
		t.Errorf("expected not-ok result for empty file, got ok: %s", res.Msg)
	}
	if !strings.Contains(res.Msg, "skipped empty file") {
		t.Errorf("result message = %q, want skipped empty file", res.Msg)
	}
	if hit {
		t.Errorf("server was contacted for empty file; expected no upload")
	}
}

func TestHeapDumpRunPassesThroughZstdExtensions(t *testing.T) {
	prevOnlyCapture := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.OnlyCapture = false
	defer func() { config.GlobalConfig.OnlyCapture = prevOnlyCapture }()

	raw := []byte("already compressed heap dump source payload")
	var compressed bytes.Buffer
	enc, err := newZstdEncoder(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	compressedBody := compressed.Bytes()

	tests := []struct {
		name string
		ext  string
	}{
		{name: "zst", ext: ".zst"},
		{name: "zstd", ext: ".zstd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			srcDir := filepath.Join(tmpDir, "src")
			if err := os.Mkdir(srcDir, 0755); err != nil {
				t.Fatal(err)
			}
			srcPath := filepath.Join(srcDir, "heap"+tt.ext)
			if err := os.WriteFile(srcPath, compressedBody, 0644); err != nil {
				t.Fatal(err)
			}

			captureDir := filepath.Join(tmpDir, "capture")
			if err := os.Mkdir(captureDir, 0755); err != nil {
				t.Fatal(err)
			}
			oldWd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(captureDir); err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := os.Chdir(oldWd); err != nil {
					t.Fatalf("restore working directory: %v", err)
				}
			}()

			var (
				gotEncoding string
				gotBody     []byte
			)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEncoding = r.URL.Query().Get("Content-Encoding")
				var readErr error
				gotBody, readErr = io.ReadAll(r.Body)
				if readErr != nil {
					http.Error(w, readErr.Error(), http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			hd := NewHeapDump("", 0, srcPath, false)
			hd.SetEndpoint(srv.URL + "?k=test")

			res, err := hd.Run()
			if err != nil {
				t.Fatal(err)
			}
			if !res.Ok {
				t.Fatalf("upload not ok: %s", res.Msg)
			}
			if gotEncoding != "zst" {
				t.Errorf("Content-Encoding = %q, want %q", gotEncoding, "zst")
			}
			if !bytes.Equal(gotBody, compressedBody) {
				t.Errorf("uploaded body mismatch: got %d bytes, want %d", len(gotBody), len(compressedBody))
			}
			copiedPath := filepath.Join(captureDir, "heap_dump"+tt.ext)
			copiedBody, err := os.ReadFile(copiedPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(copiedBody, compressedBody) {
				t.Errorf("copied body mismatch: got %d bytes, want %d", len(copiedBody), len(compressedBody))
			}
		})
	}
}
