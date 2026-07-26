package capture

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/config"
)

func writeTempDmp(t *testing.T, content []byte) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "heap_dump.dmp")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestDotnetHeapDumpUploadCapturedFile(t *testing.T) {
	t.Run("uploads to the hd receiver as zst when online", func(t *testing.T) {
		prev := config.GlobalConfig.OnlyCapture
		config.GlobalConfig.OnlyCapture = false
		defer func() { config.GlobalConfig.OnlyCapture = prev }()

		var gotDt, gotEncoding string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotDt = r.URL.Query().Get("dt")
			gotEncoding = r.URL.Query().Get("Content-Encoding")
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		d := &DotnetHeapDump{Pid: 0}
		d.SetEndpoint(srv.URL + "?k=test")

		res := d.UploadCapturedFile(writeTempDmp(t, bytes.Repeat([]byte("minidump\x00"), 1024)))
		if !res.Ok {
			t.Fatalf("upload not ok: %s", res.Msg)
		}
		if gotDt != "hd" {
			t.Errorf("dt = %q, want %q", gotDt, "hd")
		}
		if gotEncoding != "zst" {
			t.Errorf("Content-Encoding = %q, want %q", gotEncoding, "zst")
		}
	})

	t.Run("skips upload in only-capture mode", func(t *testing.T) {
		prev := config.GlobalConfig.OnlyCapture
		config.GlobalConfig.OnlyCapture = true
		defer func() { config.GlobalConfig.OnlyCapture = prev }()

		hit := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit = true
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		d := &DotnetHeapDump{Pid: 0}
		d.SetEndpoint(srv.URL + "?k=test")

		res := d.UploadCapturedFile(writeTempDmp(t, []byte("minidump")))
		if res.Ok {
			t.Errorf("expected not-ok result in only-capture mode, got ok: %s", res.Msg)
		}
		if hit {
			t.Error("server was contacted in only-capture mode; expected no upload")
		}
	})
}
