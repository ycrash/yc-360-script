//go:build !windows

package capture

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"yc-agent/internal/config"
)

// Gated to non-Windows: the yc-dot-net-*.exe stub these tests exec is a /bin/sh script.

// Above pid_max on both Linux (4194304) and Darwin (99998).
const nonexistentPid = 1 << 30

func writeDotnetStub(t *testing.T) string {
	t.Helper()
	stub := filepath.Join(t.TempDir(), "yc-dot-net-stub.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > stub_args.txt\n" +
		"echo 'stub: diagnostic on stderr' 1>&2\n" +
		"case \"$1\" in\n" +
		"  -hd) out=\"heap_dump.dmp\" ;;\n" +
		"  -hdsub) out=\"heap_stats_$2.json\" ;;\n" +
		"  *) out=\"unknown.out\" ;;\n" +
		"esac\n" +
		"case \"$YC_STUB_MODE\" in\n" +
		"  exit-nonzero) exit 3 ;;\n" +
		"  no-output) exit 0 ;;\n" +
		"  empty-output) : > \"$out\"; exit 0 ;;\n" +
		"esac\n" +
		"printf 'STUBHEAPDUMP' > \"$out\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return stub
}

func useDotnetStub(t *testing.T, onlyCapture bool) string {
	t.Helper()

	prevTool := config.GlobalConfig.DotnetToolPath
	prevOnly := config.GlobalConfig.OnlyCapture
	config.GlobalConfig.DotnetToolPath = writeDotnetStub(t)
	config.GlobalConfig.OnlyCapture = onlyCapture
	t.Cleanup(func() {
		config.GlobalConfig.DotnetToolPath = prevTool
		config.GlobalConfig.OnlyCapture = prevOnly
	})

	t.Chdir(t.TempDir())

	resolved, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return resolved
}

func readStubArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile("stub_args.txt")
	if err != nil {
		t.Fatalf("read stub args: %v", err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

func TestDotnetHeapDumpCaptureArgs(t *testing.T) {
	workDir := useDotnetStub(t, false)

	d := &DotnetHeapDump{Pid: 4321}
	f, err := d.CaptureToFile()
	if err != nil {
		t.Fatalf("CaptureToFile: %v", err)
	}
	defer f.Close()

	if got := filepath.Base(f.Name()); got != "heap_dump.dmp" {
		t.Errorf("captured file = %q, want heap_dump.dmp", got)
	}

	args := readStubArgs(t)
	want := []string{"-hd", "4321", workDir}
	if len(args) != len(want) {
		t.Fatalf("stub argv = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestDotnetHeapDumpCaptureErrors(t *testing.T) {
	tests := []struct {
		name     string
		stubMode string
		wantErr  string
	}{
		{"tool exits non-zero", "exit-nonzero", "dotnet tool execution failed"},
		{"no output file", "no-output", "was not created"},
		{"empty output file", "empty-output", "created empty output file"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useDotnetStub(t, false)
			t.Setenv("YC_STUB_MODE", tt.stubMode)

			f, err := (&DotnetHeapDump{Pid: 4321}).CaptureToFile()
			if err == nil {
				f.Close()
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestDotnetHeapDumpOnlyCaptureKeepsDmpName(t *testing.T) {
	useDotnetStub(t, true)

	d := &DotnetHeapDump{Pid: 4321}
	f, err := d.CaptureToFile()
	if err != nil {
		t.Fatalf("CaptureToFile: %v", err)
	}
	defer f.Close()

	if got := filepath.Base(f.Name()); got != "heap_dump.dmp" {
		t.Errorf("only-capture full dump renamed to %q, want heap_dump.dmp", got)
	}
	if _, err := os.Stat("hdsub.out"); err == nil {
		t.Error("full dump must not be renamed to hdsub.out")
	}
}

func TestDotnetHeapSubstituteOnlyCaptureRenamed(t *testing.T) {
	useDotnetStub(t, true)

	d := &DotnetHDSub{Pid: 4321}
	f, err := d.CaptureToFile()
	if err != nil {
		t.Fatalf("CaptureToFile: %v", err)
	}
	defer f.Close()

	if args := readStubArgs(t); len(args) == 0 || args[0] != "-hdsub" {
		t.Errorf("substitute invoked with %v, want first arg -hdsub", args)
	}
	if got := filepath.Base(f.Name()); got != "hdsub.out" {
		t.Errorf("substitute artifact = %q, want hdsub.out", got)
	}
}

func TestDotnetHeapDumpRunMissingProcess(t *testing.T) {
	useDotnetStub(t, false)

	_, err := (&DotnetHeapDump{Pid: nonexistentPid}).Run()
	if err == nil {
		t.Fatal("expected an error for a nonexistent process, got nil")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("error = %v, want it to contain %q", err, "does not exist")
	}
	if _, statErr := os.Stat("stub_args.txt"); statErr == nil {
		t.Error("helper was invoked for a nonexistent process")
	}
}

func TestDotnetHeapDumpRunKeepsLocalDmp(t *testing.T) {
	t.Run("kept after online upload", func(t *testing.T) {
		useDotnetStub(t, false)

		var gotEncoding string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotEncoding = r.URL.Query().Get("Content-Encoding")
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		d := &DotnetHeapDump{Pid: os.Getpid()}
		d.SetEndpoint(srv.URL + "?k=test")

		res, err := d.Run()
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Ok {
			t.Fatalf("upload not ok: %s", res.Msg)
		}
		if gotEncoding != "zst" {
			t.Errorf("Content-Encoding = %q, want %q", gotEncoding, "zst")
		}
		if _, err := os.Stat("heap_dump.dmp"); err != nil {
			t.Errorf("expected heap_dump.dmp to remain after online upload: %v", err)
		}
	})

	t.Run("kept in only-capture mode", func(t *testing.T) {
		useDotnetStub(t, true)

		d := &DotnetHeapDump{Pid: os.Getpid()}
		d.SetEndpoint("http://127.0.0.1:0?k=test")

		res, err := d.Run()
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Ok {
			t.Errorf("expected not-ok result in only-capture mode, got ok: %s", res.Msg)
		}
		if _, err := os.Stat("heap_dump.dmp"); err != nil {
			t.Errorf("expected heap_dump.dmp to remain in only-capture mode: %v", err)
		}
	})
}
