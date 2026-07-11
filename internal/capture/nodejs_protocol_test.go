package capture

import (
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/config"
)

func TestReadNodeRegistration(t *testing.T) {
	dir := t.TempDir()
	pid := 4242
	good := `{"pid":4242,"pipePath":"/tmp/yc.sock","nodeVersion":"v18.0.0","platform":"linux","startedAt":"2026-07-07T00:00:00Z"}`
	if err := os.WriteFile(nodeRegistrationPath(dir, pid), []byte(good), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := ReadNodeRegistration(dir, pid)
	if err != nil {
		t.Fatalf("ReadNodeRegistration: %v", err)
	}
	if reg.PipePath != "/tmp/yc.sock" || reg.NodeVersion != "v18.0.0" {
		t.Errorf("unexpected registration: %+v", reg)
	}
}

func TestReadNodeRegistrationStalePID(t *testing.T) {
	dir := t.TempDir()
	// File claims pid 111 but we ask for 4242 → stale / PID reuse.
	if err := os.WriteFile(nodeRegistrationPath(dir, 4242), []byte(`{"pid":111,"pipePath":"/tmp/x.sock"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNodeRegistration(dir, 4242); err == nil {
		t.Fatalf("expected error for mismatched pid, got nil")
	}
}

func TestReadNodeRegistrationMissingPipePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(nodeRegistrationPath(dir, 5), []byte(`{"pid":5}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadNodeRegistration(dir, 5); err == nil {
		t.Fatalf("expected error for missing pipePath, got nil")
	}
}

func TestNodeTokenCacheAndInvalidate(t *testing.T) {
	dir := t.TempDir()
	tokenPath := filepath.Join(dir, nodeTokenFileName)
	if err := os.WriteFile(tokenPath, []byte("  secret-old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	InvalidateNodeToken()
	got, err := NodeToken(dir)
	if err != nil {
		t.Fatalf("NodeToken: %v", err)
	}
	if got != "secret-old" {
		t.Errorf("token = %q, want secret-old (trimmed)", got)
	}

	// Rewrite the file; without invalidation the cache should still return old.
	if err := os.WriteFile(tokenPath, []byte("secret-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := NodeToken(dir); got != "secret-old" {
		t.Errorf("cached token = %q, want secret-old", got)
	}

	InvalidateNodeToken()
	if got, _ := NodeToken(dir); got != "secret-new" {
		t.Errorf("after invalidate token = %q, want secret-new", got)
	}
}

func TestNodeRuntimeDirResolution(t *testing.T) {
	origDir := config.GlobalConfig.NodejsRuntimeDir
	origEnv, hadEnv := os.LookupEnv(nodeRuntimeDirEnv)
	t.Cleanup(func() {
		config.GlobalConfig.NodejsRuntimeDir = origDir
		if hadEnv {
			os.Setenv(nodeRuntimeDirEnv, origEnv)
		} else {
			os.Unsetenv(nodeRuntimeDirEnv)
		}
	})

	// 1. Config flag wins.
	config.GlobalConfig.NodejsRuntimeDir = "/opt/from-flag"
	os.Setenv(nodeRuntimeDirEnv, "/opt/from-env")
	if got := NodeRuntimeDir(); got != "/opt/from-flag" {
		t.Errorf("config flag should win, got %q", got)
	}

	// 2. Env var when no flag.
	config.GlobalConfig.NodejsRuntimeDir = ""
	if got := NodeRuntimeDir(); got != "/opt/from-env" {
		t.Errorf("env should win, got %q", got)
	}

	// 3. Default under tmpdir.
	os.Unsetenv(nodeRuntimeDirEnv)
	want := filepath.Join(os.TempDir(), "yc360", "node")
	if got := NodeRuntimeDir(); got != want {
		t.Errorf("default = %q, want %q", got, want)
	}
}
