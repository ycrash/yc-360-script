package capture

import (
	"os"
	"testing"

	"yc-agent/internal/config"
)

// withNodeConfig points discovery at dir and sets the capture mode for the
// duration of a test, restoring the globals afterward.
func withNodeConfig(t *testing.T, mode, runtimeDir string) {
	t.Helper()
	origMode := config.GlobalConfig.NodejsCaptureMode
	origDir := config.GlobalConfig.NodejsRuntimeDir
	origEnv, hadEnv := os.LookupEnv(nodeRuntimeDirEnv)
	t.Cleanup(func() {
		config.GlobalConfig.NodejsCaptureMode = origMode
		config.GlobalConfig.NodejsRuntimeDir = origDir
		if hadEnv {
			os.Setenv(nodeRuntimeDirEnv, origEnv)
		} else {
			os.Unsetenv(nodeRuntimeDirEnv)
		}
	})
	config.GlobalConfig.NodejsCaptureMode = mode
	config.GlobalConfig.NodejsRuntimeDir = runtimeDir
	// Ensure the env var never leaks a real runtime dir into the test.
	os.Unsetenv(nodeRuntimeDirEnv)
}

func writeNodeRegistration(t *testing.T, dir string, filePID int, body string) {
	t.Helper()
	if err := os.WriteFile(nodeRegistrationPath(dir, filePID), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestResolveNodeCaptureNoRegistration(t *testing.T) {
	dir := t.TempDir() // empty — no registration file
	withNodeConfig(t, "hook", dir)

	ctx := ResolveNodeCapture(9999)
	if ctx.Mode != NodeCaptureModeHook {
		t.Errorf("Mode = %q, want hook", ctx.Mode)
	}
	if ctx.HookAvailable() {
		t.Errorf("HookAvailable() = true, want false when no registration exists")
	}
	if ctx.HookErr == nil {
		t.Errorf("HookErr = nil, want a non-nil error (actionable message path)")
	}
}

// A registration whose socket has no live listener is NOT a usable hook: since
// item #3, ResolveNodeCapture pings to confirm liveness. The responsive case
// (live socket) and the explicit dead-socket case live in nodejs_ipc_test.go,
// which has the fake-hook server to exercise them.

func TestResolveNodeCaptureStalePID(t *testing.T) {
	dir := t.TempDir()
	withNodeConfig(t, "hook", dir)
	// Registration file for pid 4242 but its pid field claims 111 → stale.
	writeNodeRegistration(t, dir, 4242, `{"pid":111,"pipePath":"/tmp/x.sock"}`)

	ctx := ResolveNodeCapture(4242)
	if ctx.HookAvailable() {
		t.Errorf("HookAvailable() = true, want false for a stale (pid-mismatched) registration")
	}
	if ctx.HookErr == nil {
		t.Errorf("HookErr = nil, want a non-nil error for a stale registration")
	}
}

func TestResolveNodeCaptureSignalMode(t *testing.T) {
	if IsWindows() {
		t.Skip("signal mode is refused on Windows; covered by the Windows-refuse path")
	}
	withNodeConfig(t, "signal", t.TempDir())

	ctx := ResolveNodeCapture(4242)
	if ctx.Mode != NodeCaptureModeSignal {
		t.Errorf("Mode = %q, want signal", ctx.Mode)
	}
	if ctx.HookAvailable() {
		t.Errorf("HookAvailable() = true, want false in signal mode")
	}
	if ctx.HookErr != nil {
		t.Errorf("HookErr = %v, want nil in signal mode on POSIX", ctx.HookErr)
	}
}

func TestNormalizeNodeCaptureMode(t *testing.T) {
	cases := map[string]string{
		"":         NodeCaptureModeHook,
		"  ":       NodeCaptureModeHook,
		"hook":     NodeCaptureModeHook,
		"HOOK":     NodeCaptureModeHook,
		" Signal ": NodeCaptureModeSignal,
		"signal":   NodeCaptureModeSignal,
	}
	for in, want := range cases {
		if got := NormalizeNodeCaptureMode(in); got != want {
			t.Errorf("NormalizeNodeCaptureMode(%q) = %q, want %q", in, got, want)
		}
	}
}
