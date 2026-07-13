package capture

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestReshapeNodeReportToProcessOverview(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.out")
	full := `{
		"header": {"processId": 1},
		"javascriptStack": {"message": "x", "stack": ["at a", "at b"]},
		"nativeStack": [{"pc": "0x1"}],
		"sharedObjects": ["libc"],
		"workers": [{"threadId": 1}],
		"environmentVariables": {"SECRET_TOKEN": "hunter2"},
		"libuv": [],
		"resourceUsage": {}
	}`
	if err := os.WriteFile(path, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := reshapeNodeReportToProcessOverview(path); err != nil {
		t.Fatalf("reshape: %v", err)
	}

	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("reshaped output not valid JSON: %v", err)
	}

	// callStack is the flat frame array; the javascriptStack wrapper is gone.
	stack, ok := got["callStack"].([]any)
	if !ok || len(stack) != 2 || stack[0] != "at a" {
		t.Errorf("callStack = %v, want [\"at a\",\"at b\"]", got["callStack"])
	}

	// Heavy fields are dropped.
	for _, k := range []string{"javascriptStack", "nativeStack", "sharedObjects", "workers"} {
		if _, present := got[k]; present {
			t.Errorf("field %q should have been removed from the process overview", k)
		}
	}

	// environmentVariables survives reshaping, but sensitive-looking values are
	// masked in place (same policy as the hook's own maskSensitiveEnvVars) since
	// --report-on-signal writes them unmasked.
	envVars, ok := got["environmentVariables"].(map[string]any)
	if !ok {
		t.Fatalf("environmentVariables should survive reshaping, got %v", got["environmentVariables"])
	}
	if got := envVars["SECRET_TOKEN"]; got != "*******" {
		t.Errorf("SECRET_TOKEN = %v, want fully masked \"*******\"", got)
	}

	// Kept fields survive.
	if _, ok := got["header"].(map[string]any); !ok {
		t.Errorf("header should be preserved")
	}
}

func TestReshapeNodeReportToProcessOverviewJavascriptStackShapes(t *testing.T) {
	cases := []struct {
		name            string
		javascriptStack string // raw JSON for the javascriptStack field, or "" to omit it
		wantCallStack   any    // expected callStack: nil, or []any{...}
	}{
		{
			name:            "idle event loop has no stack",
			javascriptStack: `{"message":"","errorProperties":{}}`,
			wantCallStack:   nil,
		},
		{
			name:            "proper stack array",
			javascriptStack: `{"stack":["at a","at b"]}`,
			wantCallStack:   []any{"at a", "at b"},
		},
		{
			name:            "javascriptStack absent entirely",
			javascriptStack: "",
			wantCallStack:   nil,
		},
		{
			name:            "stack present but not an array",
			javascriptStack: `{"stack":"not-an-array"}`,
			wantCallStack:   nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "report.out")

			// A full signal-mode-shaped report, always carrying the heavy fields the
			// reshape must drop, plus environmentVariables (which the reshape masks
			// in place rather than drops) - checked on every shape.
			report := map[string]any{
				"header":               map[string]any{"processId": 1},
				"nativeStack":          []any{map[string]any{"pc": "0x1"}},
				"sharedObjects":        []any{"libc"},
				"workers":              []any{map[string]any{"threadId": 1}},
				"environmentVariables": map[string]any{"SECRET_TOKEN": "hunter2"},
			}
			if tc.javascriptStack != "" {
				var js any
				if err := json.Unmarshal([]byte(tc.javascriptStack), &js); err != nil {
					t.Fatalf("bad javascriptStack fixture: %v", err)
				}
				report["javascriptStack"] = js
			}
			raw, _ := json.Marshal(report)
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}

			if err := reshapeNodeReportToProcessOverview(path); err != nil {
				t.Fatalf("reshape: %v", err)
			}

			data, _ := os.ReadFile(path)
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("reshaped output not valid JSON: %v", err)
			}

			// callStack must always be present, holding either the frame array or null.
			cs, present := got["callStack"]
			if !present {
				t.Fatalf("callStack key missing from output")
			}
			if !reflect.DeepEqual(cs, tc.wantCallStack) {
				t.Errorf("callStack = %#v, want %#v", cs, tc.wantCallStack)
			}

			// Heavy fields are always dropped, even on the unexpected-shape path.
			for _, k := range []string{"javascriptStack", "nativeStack", "sharedObjects", "workers"} {
				if _, ok := got[k]; ok {
					t.Errorf("field %q should have been removed", k)
				}
			}

			// environmentVariables survives, masked, regardless of javascriptStack shape.
			envVars, ok := got["environmentVariables"].(map[string]any)
			if !ok {
				t.Fatalf("environmentVariables should survive reshaping, got %v", got["environmentVariables"])
			}
			if got := envVars["SECRET_TOKEN"]; got != "*******" {
				t.Errorf("SECRET_TOKEN = %v, want fully masked \"*******\"", got)
			}
		})
	}
}

func TestNodeReportValidAgainstCrashFixture(t *testing.T) {
	// Resolve fixtures via the module root rather than a CWD-relative path: the
	// capture package's test init (heap_test.go) chdirs into testdata/, so a bare
	// or "testdata/"-prefixed path would not resolve reliably. (repoRoot lives in
	// nodejs_dt_test.go, same package.)
	testdata := filepath.Join(repoRoot(t), "internal", "capture", "testdata")

	// The realistic truncated crash sample must be rejected.
	if NodeReportValid(filepath.Join(testdata, "node-threaddump-crashed-truncated.out")) {
		t.Errorf("truncated crash-sample report must be rejected as invalid JSON")
	}
	// A known-good process-overview sample must pass.
	if !NodeReportValid(filepath.Join(testdata, "node-processoverview.out")) {
		t.Errorf("well-formed process-overview sample must pass")
	}
}

func TestNormalizeNodeSignalNameMessages(t *testing.T) {
	for _, name := range []string{"SIGUSR1", "USR1", "sigusr1"} {
		_, err := NormalizeNodeSignalName(name)
		if err == nil {
			t.Fatalf("%q must be refused", name)
		}
		if !strings.Contains(err.Error(), "inspector") {
			t.Errorf("%q refusal = %q, want the inspector/security reason (not the generic 'unsupported')", name, err)
		}
	}

	// A genuinely unsupported signal gets the distinct 'unsupported' message.
	if _, err := NormalizeNodeSignalName("SIGTERM"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Errorf("SIGTERM error = %v, want an 'unsupported signal' message", err)
	}
	// Empty is its own message.
	if _, err := NormalizeNodeSignalName(""); err == nil || !strings.Contains(err.Error(), "must be set") {
		t.Errorf("empty error = %v, want a 'must be set' message", err)
	}
	// A supported signal canonicalizes.
	if got, err := NormalizeNodeSignalName("usr2"); err != nil || got != "SIGUSR2" {
		t.Errorf("usr2 -> (%q, %v), want (SIGUSR2, nil)", got, err)
	}
}
