//go:build !windows

package capture

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"yc-agent/internal/config"
)

type recordedUpload struct {
	dt      string
	rawURL  string
	bodyLen int
}

type fakeReceiver struct {
	*httptest.Server
	mu      sync.Mutex
	uploads []recordedUpload
}

func newFakeReceiver(t *testing.T) *fakeReceiver {
	t.Helper()
	fr := &fakeReceiver{}
	fr.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fr.mu.Lock()
		fr.uploads = append(fr.uploads, recordedUpload{
			dt:      r.URL.Query().Get("dt"),
			rawURL:  r.URL.String(),
			bodyLen: len(body),
		})
		fr.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK")
	}))
	t.Cleanup(fr.Server.Close)
	return fr
}

func (fr *fakeReceiver) endpoint() string { return fr.Server.URL + "?de=test" }

func (fr *fakeReceiver) dtCodes() []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	out := make([]string, len(fr.uploads))
	for i, u := range fr.uploads {
		out[i] = u.dt
	}
	return out
}

func newHookCaptureContext(t *testing.T, fh *fakeHook) *NodeCaptureContext {
	t.Helper()
	client, err := NewNodeHookClient(fh.dir, fh.pid)
	if err != nil {
		t.Fatalf("NewNodeHookClient: %v", err)
	}
	return &NodeCaptureContext{PID: fh.pid, RuntimeDir: fh.dir, Mode: NodeCaptureModeHook, Client: client}
}

func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn helper process for a dead PID: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait()
	if IsProcessExists(pid) {
		t.Skipf("reaped helper PID %d still reported alive; cannot obtain a dead PID deterministically", pid)
	}
	return pid
}

func TestNodeProcessOverviewRetryLoop(t *testing.T) {
	t.Run("retry then succeed", func(t *testing.T) {
		fh := startFakeHook(t, 41001, "tok")
		fh.overviewInvalidUntil.Store(1) // invalid on attempt 1, valid on attempt 2+
		fr := newFakeReceiver(t)

		task := &NodeProcessOverview{Pid: os.Getpid(), Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}
		task.SetEndpoint(fr.endpoint())

		res, err := task.Run()
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Ok {
			t.Errorf("expected Ok after retry-then-succeed, got Ok=false msg=%q", res.Msg)
		}
		if got := fh.poCallCount.Load(); got != 2 {
			t.Errorf("dumpProcessOverview call count = %d, want 2 (one retry)", got)
		}
		if dts := fr.dtCodes(); len(dts) != 1 || dts[0] != nodeDTProcessOverview {
			t.Errorf("uploaded dt codes = %v, want exactly [%q]", dts, nodeDTProcessOverview)
		}
		if !NodeReportValid(filepath.Join(task.OutDir, NodeProcessOverviewFileName)) {
			t.Errorf("final process-overview file should be valid JSON")
		}
	})

	t.Run("invalid exhausted", func(t *testing.T) {
		fh := startFakeHook(t, 41002, "tok")
		fh.overviewInvalidUntil.Store(99) // invalid on every attempt

		task := &NodeProcessOverview{Pid: os.Getpid(), Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}

		res, _ := task.Run()
		if res.Ok {
			t.Errorf("expected failure after exhausting retries")
		}
		wantMsg := fmt.Sprintf("node process overview for pid %d is not well-formed JSON after 2 attempts", os.Getpid())
		if res.Msg != wantMsg {
			t.Errorf("Msg = %q, want %q", res.Msg, wantMsg)
		}
		if got := fh.poCallCount.Load(); got != 2 {
			t.Errorf("call count = %d, want 2", got)
		}
	})

	t.Run("invalid and process dead skips retry", func(t *testing.T) {
		fh := startFakeHook(t, 41003, "tok")
		fh.overviewInvalidUntil.Store(99)
		dead := deadPid(t)

		task := &NodeProcessOverview{Pid: dead, Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}

		// Run()'s top guard rejects an already-dead PID before the loop, so to
		// exercise the loop's mid-capture death branch we call runHook directly
		// with the same PID (which IsProcessExists reports as dead).
		outPath := filepath.Join(task.OutDir, NodeProcessOverviewFileName)
		res, _ := task.runHook(outPath)
		if res.Ok {
			t.Errorf("expected failure when the process died mid-capture")
		}
		if !strings.Contains(res.Msg, "died during process overview capture (truncated/invalid report)") {
			t.Errorf("Msg = %q, want the truncated-death variant", res.Msg)
		}
		if got := fh.poCallCount.Load(); got != 1 {
			t.Errorf("call count = %d, want 1 (no retry after death)", got)
		}
	})

	t.Run("rpc error then succeed", func(t *testing.T) {
		fh := startFakeHook(t, 41005, "tok")
		fh.overviewRPCErrorUntil.Store(1) // RPC fails on attempt 1, ok on attempt 2 (process alive)
		fr := newFakeReceiver(t)

		task := &NodeProcessOverview{Pid: os.Getpid(), Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}
		task.SetEndpoint(fr.endpoint())

		res, err := task.Run()
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if !res.Ok {
			t.Errorf("expected Ok after RPC-error-then-succeed, got Ok=false msg=%q", res.Msg)
		}
		if got := fh.poCallCount.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (retried after the RPC error)", got)
		}
		if dts := fr.dtCodes(); len(dts) != 1 || dts[0] != nodeDTProcessOverview {
			t.Errorf("uploaded dt codes = %v, want exactly [%q]", dts, nodeDTProcessOverview)
		}
	})

	t.Run("rpc error exhausted while alive", func(t *testing.T) {
		fh := startFakeHook(t, 41006, "tok")
		fh.overviewRPCErrorUntil.Store(99) // RPC fails on every attempt, process stays alive

		task := &NodeProcessOverview{Pid: os.Getpid(), Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}

		res, _ := task.Run()
		if res.Ok {
			t.Errorf("expected failure after exhausting retries on a persistent RPC error")
		}
		// The final failure surfaces the RPC error itself (not a death/JSON message).
		if !strings.Contains(res.Msg, "dumpProcessOverview failed") {
			t.Errorf("Msg = %q, want the underlying RPC error", res.Msg)
		}
		if got := fh.poCallCount.Load(); got != 2 {
			t.Errorf("call count = %d, want 2 (one retry)", got)
		}
	})

	t.Run("rpc error and process dead", func(t *testing.T) {
		fh := startFakeHook(t, 41004, "tok")
		fh.overviewRPCErrorUntil.Store(99)
		dead := deadPid(t)

		task := &NodeProcessOverview{Pid: dead, Ctx: newHookCaptureContext(t, fh), OutDir: t.TempDir()}

		outPath := filepath.Join(task.OutDir, NodeProcessOverviewFileName)
		res, _ := task.runHook(outPath)
		if res.Ok {
			t.Errorf("expected failure on RPC error + dead process")
		}
		if !strings.Contains(res.Msg, "died during process overview capture") {
			t.Errorf("Msg = %q, want the death message", res.Msg)
		}
		if strings.Contains(res.Msg, "truncated/invalid report") {
			t.Errorf("Msg = %q should be the non-truncated death variant (RPC error, not invalid JSON)", res.Msg)
		}
		if got := fh.poCallCount.Load(); got != 1 {
			t.Errorf("call count = %d, want 1", got)
		}
	})
}

func TestNodeUnhandledRejectionsPopulated(t *testing.T) {
	fh := startFakeHook(t, 42001, "tok")
	fr := newFakeReceiver(t)
	outDir := t.TempDir()

	task := &NodeUnhandledRejections{Pid: os.Getpid(), Ctx: newHookCaptureContext(t, fh), OutDir: outDir}
	task.SetEndpoint(fr.endpoint())

	res, err := task.Run()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Ok {
		t.Fatalf("expected Ok, got Ok=false msg=%q", res.Msg)
	}
	// Uploaded under dt=noderej — never dt=td, never a placeholder collision.
	if dts := fr.dtCodes(); len(dts) != 1 || dts[0] != nodeDTUnhandledRejections {
		t.Errorf("uploaded dt codes = %v, want exactly [%q]", dts, nodeDTUnhandledRejections)
	}

	// The wired task persisted a populated array (not the empty [] the darwin run
	// saw); the field shape itself is produced by the hook.
	data, err := os.ReadFile(filepath.Join(outDir, NodeUnhandledRejectionsFileName))
	if err != nil {
		t.Fatalf("read rejections file: %v", err)
	}
	var events []map[string]any
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("rejections file not a valid JSON array: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
	if events[0]["reason"] != "x" {
		t.Errorf("reason = %v, want \"x\"", events[0]["reason"])
	}
	if _, ok := events[0]["epochMs"]; !ok {
		t.Errorf("event missing epochMs field: %v", events[0])
	}
	if _, ok := events[0]["stackTrace"]; !ok {
		t.Errorf("event missing stackTrace field: %v", events[0])
	}
}

func TestNodeGCDumpGCDurationClamp(t *testing.T) {
	fh := startFakeHook(t, 43001, "tok")
	fh.gcNoSleep.Store(true)
	ctx := newHookCaptureContext(t, fh)

	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })
	config.GlobalConfig.OnlyCapture = true // avoid any HTTP upload; we only assert the sent window

	// An (empty) stdout file so captureViaDumpGC's stat + split succeed.
	stdoutPath := filepath.Join(t.TempDir(), "stdout.log")
	if err := os.WriteFile(stdoutPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		cfg    time.Duration
		wantMs int64
	}{
		{"over cap clamps to 60s", 120 * time.Second, nodeMaxDumpGCMs},
		{"non-positive defaults to 30s", 0, 30000},
		{"in range passes through", 45 * time.Second, 45000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			config.GlobalConfig.NodejsGCCaptureDuration = config.Duration(tc.cfg)
			gcOut := filepath.Join(t.TempDir(), NodeGCLogFileName)
			g := &NodeGC{Pid: os.Getpid(), Ctx: ctx}
			g.captureViaDumpGC(stdoutPath, gcOut)
			if got := fh.gcRecordedDurationMs.Load(); got != tc.wantMs {
				t.Errorf("dumpGC durationMs = %d, want %d", got, tc.wantMs)
			}
		})
	}
}
