package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func emptyPathEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func TestFindProcDump(t *testing.T) {
	t.Run("PROCDUMP_PATH directory containing procdump", func(t *testing.T) {
		emptyPathEnv(t)
		dir := t.TempDir()
		want := filepath.Join(dir, ProcDumpName64)
		if err := os.WriteFile(want, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROCDUMP_PATH", dir)

		got, ok := FindProcDump()
		if !ok {
			t.Fatalf("expected ProcDump to be found in %s", dir)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("PROCDUMP_PATH pointing directly at the executable", func(t *testing.T) {
		emptyPathEnv(t)
		file := filepath.Join(t.TempDir(), ProcDumpName64)
		if err := os.WriteFile(file, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROCDUMP_PATH", file)

		got, ok := FindProcDump()
		if !ok {
			t.Fatalf("expected ProcDump to be found at %s", file)
		}
		if got != file {
			t.Fatalf("got %q, want %q", got, file)
		}
	})

	t.Run("PROCDUMP_PATH prefers the 64-bit executable", func(t *testing.T) {
		emptyPathEnv(t)
		dir := t.TempDir()
		for _, name := range []string{ProcDumpName64, ProcDumpName32} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PROCDUMP_PATH", dir)

		got, ok := FindProcDump()
		if !ok {
			t.Fatalf("expected ProcDump to be found in %s", dir)
		}
		if want := filepath.Join(dir, ProcDumpName64); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to PATH when PROCDUMP_PATH does not exist", func(t *testing.T) {
		pathDir := t.TempDir()
		t.Setenv("PATH", pathDir)
		want := filepath.Join(pathDir, ProcDumpName32)
		if err := os.WriteFile(want, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROCDUMP_PATH", filepath.Join(t.TempDir(), "missing"))

		got, ok := FindProcDump()
		if !ok {
			t.Fatalf("expected ProcDump to be found on PATH in %s", pathDir)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("not found anywhere", func(t *testing.T) {
		emptyPathEnv(t)
		t.Setenv("PROCDUMP_PATH", "")

		if got, ok := FindProcDump(); ok {
			t.Fatalf("expected ProcDump not to be found, got %q", got)
		}
	})
}

func TestValidateProcDumpInstall(t *testing.T) {
	t.Run("returns no warning when found", func(t *testing.T) {
		emptyPathEnv(t)
		file := filepath.Join(t.TempDir(), ProcDumpName64)
		if err := os.WriteFile(file, []byte("stub"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PROCDUMP_PATH", file)

		if warnings := ValidateProcDumpInstall(); len(warnings) != 0 {
			t.Fatalf("expected no warnings, got %v", warnings)
		}
	})

	t.Run("returns actionable warning when missing", func(t *testing.T) {
		emptyPathEnv(t)
		t.Setenv("PROCDUMP_PATH", "")

		warnings := ValidateProcDumpInstall()
		if len(warnings) != 1 {
			t.Fatalf("expected exactly one warning, got %v", warnings)
		}
		if !strings.Contains(warnings[0], "ProcDump not found") {
			t.Fatalf("warning should explain ProcDump is missing, got %q", warnings[0])
		}
	})
}
