//go:build !windows

package capture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeHook is an in-test stand-in for yc360-node-hook.js: a Unix-socket server
// speaking the same newline-delimited JSON protocol, used to exercise the real
// NodeHookClient / RPC / discovery code paths without a live Node process.
type fakeHook struct {
	t   *testing.T
	ln  net.Listener
	dir string
	pid int

	acceptToken   atomic.Value // string — the token the server currently accepts
	strayBefore   atomic.Bool  // when set, ping writes a mismatched-id line first
	pingOversized atomic.Bool  // when set, ping floods > cap bytes with no newline
	pingNullID    atomic.Bool  // when set, ping replies with a raw id:null error line

	poCallCount           atomic.Int64 // dumpProcessOverview calls received
	overviewInvalidUntil  atomic.Int64 // write truncated (invalid) JSON while poCallCount <= this
	overviewRPCErrorUntil atomic.Int64 // reply ok:false (RPC failure) while poCallCount <= this
	gcCallCount           atomic.Int64 // dumpGC calls received
	gcRecordedDurationMs  atomic.Int64 // durationMs seen by the most recent dumpGC
	gcNoSleep             atomic.Bool  // skip dumpGC's window sleep (keep the test fast)

	wg sync.WaitGroup
}

type fakeInReq struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	Token  string         `json:"token"`
}

func startFakeHook(t *testing.T, pid int, token string) *fakeHook {
	t.Helper()
	// A short /tmp path keeps the socket well under the sun_path length limit
	// (t.TempDir() paths on macOS are long enough to overflow it).
	dir, err := os.MkdirTemp("/tmp", "ychook")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	sockPath := filepath.Join(dir, strconv.Itoa(pid)+".sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}

	reg := fmt.Sprintf(`{"pid":%d,"pipePath":%q,"nodeVersion":"v18.20.8","platform":%q,"startedAt":"2026-07-07T00:00:00Z"}`, pid, sockPath, runtime.GOOS)
	if err := os.WriteFile(nodeRegistrationPath(dir, pid), []byte(reg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, nodeTokenFileName), []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}

	fh := &fakeHook{t: t, ln: ln, dir: dir, pid: pid}
	fh.acceptToken.Store(token)
	fh.wg.Add(1)
	go fh.serve()

	InvalidateNodeToken()
	t.Cleanup(func() {
		ln.Close()
		fh.wg.Wait()
		os.RemoveAll(dir)
		InvalidateNodeToken()
	})
	return fh
}

func (fh *fakeHook) setAcceptToken(tok string) { fh.acceptToken.Store(tok) }

func (fh *fakeHook) serve() {
	defer fh.wg.Done()
	for {
		conn, err := fh.ln.Accept()
		if err != nil {
			return // listener closed
		}
		fh.wg.Add(1)
		go func() {
			defer fh.wg.Done()
			defer conn.Close()
			fh.handle(conn)
		}()
	}
}

func (fh *fakeHook) handle(conn net.Conn) {
	r := bufio.NewReader(conn)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return
	}
	var req fakeInReq
	if err := json.Unmarshal(line, &req); err != nil {
		writeFakeResp(conn, req.ID, false, nil, "invalid JSON request")
		return
	}
	if req.Token != fh.acceptToken.Load().(string) {
		writeFakeResp(conn, req.ID, false, nil, "unauthorized")
		return
	}

	switch req.Method {
	case "ping":
		if fh.pingOversized.Load() {
			// Flood more than the client's response cap with NO newline framing.
			// The client must abort with the cap error rather than buffer an
			// unbounded (potentially infinite) stream into memory.
			conn.Write(bytes.Repeat([]byte("x"), maxNodeResponseBytes+16))
			return
		}
		if fh.pingNullID.Load() {
			// A framing/parse error the hook reports with a literal id:null (not a
			// JSON string). The client must ACCEPT it — null decodes to "", so the
			// id-mismatch skip guard must not fire — and surface the error body.
			conn.Write([]byte(`{"id":null,"ok":false,"error":"framing boom"}` + "\n"))
			return
		}
		if fh.strayBefore.Load() {
			// A response whose id does NOT match the request. The client must
			// skip it (matching on id, not ordering) and keep reading.
			writeFakeResp(conn, "stray-"+req.ID, true, map[string]any{
				"pid": fh.pid, "nodeVersion": "vSTRAY", "platform": runtime.GOOS,
				"uptimeSec": 0.0, "hookVersion": "STRAY",
			}, "")
		}
		writeFakeResp(conn, req.ID, true, map[string]any{
			"pid": fh.pid, "nodeVersion": "v18.20.8", "platform": runtime.GOOS,
			"uptimeSec": 12.5, "hookVersion": "0.5.1-poc",
		}, "")
	case "dumpProcessOverview":
		n := fh.poCallCount.Add(1)
		outPath, _ := req.Params["outPath"].(string)
		if n <= fh.overviewRPCErrorUntil.Load() {
			// Simulate the RPC itself failing (ok:false) — no file is written.
			writeFakeResp(conn, req.ID, false, nil, "simulated dumpProcessOverview failure")
			return
		}
		body := []byte(`{"header":{"processId":1},"callStack":[],"libuv":[]}`)
		if n <= fh.overviewInvalidUntil.Load() {
			// Truncated / not well-formed JSON — the crash signature the retry
			// guard exists to catch.
			body = []byte(`{"header":`)
		}
		os.WriteFile(outPath, body, 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath}, "")
	case "dumpHeapSummary":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`{"overview":{"used_heap_size":1},"heapSpaceHistogram":[]}`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath}, "")
	case "dumpGC":
		durMs := 10.0
		if d, ok := req.Params["durationMs"].(float64); ok {
			durMs = d
		}
		fh.gcCallCount.Add(1)
		fh.gcRecordedDurationMs.Store(int64(durMs))
		// The real hook only responds after the window; sleep the (short,
		// test-sized) duration so the async read-deadline path is exercised.
		if !fh.gcNoSleep.Load() {
			time.Sleep(time.Duration(durMs) * time.Millisecond)
		}
		writeFakeResp(conn, req.ID, true, map[string]any{
			"startedAt": "t0", "endedAt": "t1", "durationMs": durMs,
			"toggledOn": true, "toggledOff": true,
		}, "")
	case "dumpCPUProfile":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`{"nodes":[],"samples":[1,2,3],"timeDeltas":[]}`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "totalSamples": 3, "windowSeconds": 1.0}, "")
	case "dumpWorkerCPUProfiles":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`{"totalWorkerCount":0,"skippedCount":0,"unresponsiveCount":0,"profiled":[]}`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "workerCount": 0, "totalWorkerCount": 0, "unresponsiveCount": 0}, "")
	case "dumpEventLoopLag":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`[{"timeLabel":"00:00:01","count":0}]`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "sampleCount": 1, "windowSeconds": req.Params["windowSeconds"]}, "")
	case "dumpUnhandledRejections":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`[{"reason":"x","stackTrace":null,"epochMs":1}]`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "eventCount": 1, "totalCount": 1, "truncated": false, "windowSeconds": req.Params["windowSeconds"]}, "")
	case "dumpModuleInventory":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`[{"name":"app.js","version":null,"fileCount":1}]`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "packageCount": 1, "fileCount": 1}, "")
	case "dumpHandleGrowth":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`[{"statesCount":{"Timeout":1},"totalCount":1,"epochMs":1}]`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "sampleCount": 1, "windowSeconds": req.Params["windowSeconds"], "intervalSeconds": req.Params["intervalSeconds"]}, "")
	case "dumpGCStats":
		outPath, _ := req.Params["outPath"].(string)
		os.WriteFile(outPath, []byte(`[{"kind":"minor","durationMs":0.3,"epochMs":1}]`), 0o644)
		writeFakeResp(conn, req.ID, true, map[string]any{"path": outPath, "eventCount": 1, "windowSeconds": req.Params["windowSeconds"]}, "")
	default:
		writeFakeResp(conn, req.ID, false, nil, "unknown method: "+req.Method)
	}
}

func writeFakeResp(conn net.Conn, id string, ok bool, result map[string]any, errMsg string) {
	resp := map[string]any{"id": id, "ok": ok}
	if ok {
		resp["result"] = result
	} else {
		resp["error"] = errMsg
	}
	b, _ := json.Marshal(resp)
	b = append(b, '\n')
	conn.Write(b)
}

func TestNodeHookPingRoundTrip(t *testing.T) {
	fh := startFakeHook(t, 34567, "tok-abc")
	client, err := NewNodeHookClient(fh.dir, fh.pid)
	if err != nil {
		t.Fatalf("NewNodeHookClient: %v", err)
	}
	if client.PipePath() == "" {
		t.Errorf("PipePath() empty, want the registration socket path")
	}

	ping, err := client.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ping.HookVersion != "0.5.1-poc" || ping.NodeVersion != "v18.20.8" || ping.PID != fh.pid {
		t.Errorf("unexpected ping: %+v", ping)
	}
}

// TestNodeHookCaptureRPCsRoundTrip drives capture RPCs against the
// fake hook: correct params in, result decoded out, artifact written to outPath.
func TestNodeHookCaptureRPCsRoundTrip(t *testing.T) {
	fh := startFakeHook(t, 34590, "tok-abc")
	client, err := NewNodeHookClient(fh.dir, fh.pid)
	if err != nil {
		t.Fatalf("NewNodeHookClient: %v", err)
	}

	po := filepath.Join(fh.dir, "processoverview.out")
	if err := client.DumpProcessOverview(po); err != nil {
		t.Fatalf("DumpProcessOverview: %v", err)
	}
	if !NodeReportValid(po) {
		t.Errorf("dumpProcessOverview output is not valid JSON")
	}

	if err := client.DumpHeapSummary(filepath.Join(fh.dir, "hdsub.out")); err != nil {
		t.Fatalf("DumpHeapSummary: %v", err)
	}

	gcRes, err := client.DumpGC(20)
	if err != nil {
		t.Fatalf("DumpGC: %v", err)
	}
	// The result is decoded from the hook's post-window reply, and durationMs is
	// echoed back — so this also proves the param reached the hook.
	if !gcRes.ToggledOn || !gcRes.ToggledOff || gcRes.DurationMs != 20 {
		t.Errorf("unexpected dumpGC result: %+v", gcRes)
	}

	cpuRes, err := client.DumpCPUProfile(filepath.Join(fh.dir, "cpuprofile.out"), 1)
	if err != nil {
		t.Fatalf("DumpCPUProfile: %v", err)
	}
	if cpuRes.TotalSamples != 3 {
		t.Errorf("cpu totalSamples = %d, want 3", cpuRes.TotalSamples)
	}

	// Diagnostic Report page RPCs.
	ellRes, err := client.DumpEventLoopLag(filepath.Join(fh.dir, "eventlooplag.out"), 1)
	if err != nil {
		t.Fatalf("DumpEventLoopLag: %v", err)
	}
	if ellRes.SampleCount != 1 {
		t.Errorf("event loop lag sampleCount = %d, want 1", ellRes.SampleCount)
	}

	urRes, err := client.DumpUnhandledRejections(filepath.Join(fh.dir, "rejections.out"), 1)
	if err != nil {
		t.Fatalf("DumpUnhandledRejections: %v", err)
	}
	if urRes.EventCount != 1 || urRes.Truncated {
		t.Errorf("unexpected unhandled-rejections result: %+v", urRes)
	}

	miRes, err := client.DumpModuleInventory(filepath.Join(fh.dir, "modules.out"))
	if err != nil {
		t.Fatalf("DumpModuleInventory: %v", err)
	}
	if miRes.PackageCount != 1 {
		t.Errorf("module inventory packageCount = %d, want 1", miRes.PackageCount)
	}

	hgRes, err := client.DumpHandleGrowth(filepath.Join(fh.dir, "handlegrowth.out"), 4, 2)
	if err != nil {
		t.Fatalf("DumpHandleGrowth: %v", err)
	}
	if hgRes.SampleCount != 1 {
		t.Errorf("handle growth sampleCount = %d, want 1", hgRes.SampleCount)
	}

	gcsRes, err := client.DumpGCStats(filepath.Join(fh.dir, "gcstats.out"), 1)
	if err != nil {
		t.Fatalf("DumpGCStats: %v", err)
	}
	if gcsRes.EventCount != 1 {
		t.Errorf("gc stats eventCount = %d, want 1", gcsRes.EventCount)
	}
}

// TestNodeDiagnosticWindowValidation proves the windowed RPCs reject out-of-range
// windows client-side (0 is rejected, not remapped to a default), and that
// dumpHandleGrowth enforces its interval rules.
func TestNodeDiagnosticWindowValidation(t *testing.T) {
	fh := startFakeHook(t, 34591, "tok")
	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	x := filepath.Join(fh.dir, "x.out")

	// 0 is rejected, not remapped to a default, for every windowed RPC.
	if _, err := client.DumpEventLoopLag(x, 0); err == nil {
		t.Errorf("DumpEventLoopLag(0) should be rejected")
	}
	if _, err := client.DumpUnhandledRejections(x, 301); err == nil {
		t.Errorf("DumpUnhandledRejections(301) should be rejected")
	}
	if _, err := client.DumpGCStats(x, 0); err == nil {
		t.Errorf("DumpGCStats(0) should be rejected")
	}

	// Handle growth: interval must be >= 1 and strictly less than the window.
	if _, err := client.DumpHandleGrowth(x, 10, 0); err == nil {
		t.Errorf("DumpHandleGrowth interval 0 should be rejected")
	}
	if _, err := client.DumpHandleGrowth(x, 5, 10); err == nil {
		t.Errorf("DumpHandleGrowth interval >= window should be rejected")
	}
}

func TestNodeHookDumpGCValidation(t *testing.T) {
	fh := startFakeHook(t, 34592, "tok")
	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	if _, err := client.DumpGC(0); err == nil {
		t.Errorf("DumpGC(0) should be rejected")
	}
	if _, err := client.DumpGC(nodeMaxDumpGCMs + 1); err == nil {
		t.Errorf("DumpGC over cap should be rejected")
	}
}

func TestNodeHookCPUProfileValidation(t *testing.T) {
	fh := startFakeHook(t, 34593, "tok")
	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	// 0 is rejected, not remapped to default.
	if _, err := client.DumpCPUProfile(filepath.Join(fh.dir, "x"), 0); err == nil {
		t.Errorf("DumpCPUProfile(0) should be rejected")
	}
	if _, err := client.DumpCPUProfile(filepath.Join(fh.dir, "x"), 301); err == nil {
		t.Errorf("DumpCPUProfile(301) should be rejected")
	}
}

// TestNodeReportValid covers the JSON well-formedness guard that a process
// overview (or signal-mode report) must pass before it is treated as a success:
// a report truncated mid-write by a crash is the exact case it must reject.
func TestNodeReportValid(t *testing.T) {
	dir := t.TempDir()

	valid := filepath.Join(dir, "valid.out")
	os.WriteFile(valid, []byte(`{"header":{"processId":1}}`), 0o644)
	if !NodeReportValid(valid) {
		t.Errorf("NodeReportValid = false for well-formed JSON")
	}

	truncated := filepath.Join(dir, "truncated.out")
	os.WriteFile(truncated, []byte(`{"header":`), 0o644)
	if NodeReportValid(truncated) {
		t.Errorf("NodeReportValid = true for truncated JSON")
	}

	empty := filepath.Join(dir, "empty.out")
	os.WriteFile(empty, nil, 0o644)
	if NodeReportValid(empty) {
		t.Errorf("NodeReportValid = true for an empty file")
	}

	if NodeReportValid(filepath.Join(dir, "does-not-exist.out")) {
		t.Errorf("NodeReportValid = true for a missing file")
	}
}

// TestNodeHookResponseIDMatching proves the client matches responses on id and
// ignores a stray line carrying a different id, rather than trusting ordering.
func TestNodeHookResponseIDMatching(t *testing.T) {
	fh := startFakeHook(t, 34575, "tok")
	fh.strayBefore.Store(true)

	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	ping, err := client.Ping()
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if ping.HookVersion != "0.5.1-poc" {
		t.Errorf("client accepted a mismatched-id response: hookVersion=%q", ping.HookVersion)
	}
}

// TestNodeHookResponseExceedsCap proves the read is bounded: a peer that streams
// more than maxNodeResponseBytes without ever framing a newline is rejected with
// the cap error rather than driving the agent to unbounded memory use.
func TestNodeHookResponseExceedsCap(t *testing.T) {
	fh := startFakeHook(t, 34576, "tok")
	fh.pingOversized.Store(true)

	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	_, err := client.Ping()
	if err == nil {
		t.Fatalf("expected a response-cap error, got nil")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("expected the byte-cap error, got: %v", err)
	}
}

// TestNodeHookNullIDErrorAccepted proves a framing-error response carrying
// id:null is accepted (not skipped as an id mismatch): its error body surfaces
// through Ping instead of the client looping past it into a read-EOF error.
func TestNodeHookNullIDErrorAccepted(t *testing.T) {
	fh := startFakeHook(t, 34577, "tok")
	fh.pingNullID.Store(true)

	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	_, err := client.Ping()
	if err == nil {
		t.Fatalf("expected the id:null framing error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "framing boom") {
		t.Errorf("expected the id:null error body to surface (not a read/skip error), got: %v", err)
	}
}

func TestNodeHookUnauthorized(t *testing.T) {
	fh := startFakeHook(t, 34570, "right-token")
	// Overwrite the token file with the wrong secret AFTER startup; the server
	// still only accepts "right-token", so every attempt is rejected.
	os.WriteFile(filepath.Join(fh.dir, nodeTokenFileName), []byte("wrong-token"), 0o600)
	InvalidateNodeToken()

	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	if _, err := client.Ping(); err == nil {
		t.Fatalf("expected unauthorized error, got nil")
	}
}

func TestNodeHookTokenRotation(t *testing.T) {
	fh := startFakeHook(t, 34571, "old-token")

	// Prime the cache with the old token.
	if tok, _ := NodeToken(fh.dir); tok != "old-token" {
		t.Fatalf("expected cached old-token, got %q", tok)
	}

	// Rotate: server now only accepts the new token, and the file is updated.
	fh.setAcceptToken("new-token")
	os.WriteFile(filepath.Join(fh.dir, nodeTokenFileName), []byte("new-token"), 0o600)

	client, _ := NewNodeHookClient(fh.dir, fh.pid)
	// First send uses the stale cached token → unauthorized → re-read → retry OK.
	if _, err := client.Ping(); err != nil {
		t.Fatalf("Ping after rotation should succeed, got %v", err)
	}
}

func TestNodeDialTimeout(t *testing.T) {
	const maxDial = 10 * time.Second
	cases := []struct {
		in   time.Duration
		want time.Duration
	}{
		{0, maxDial},                       // unset → capped
		{-1, maxDial},                      // nonsensical → capped
		{90 * time.Second, maxDial},        // long async window → dial still capped
		{3 * time.Second, 3 * time.Second}, // within cap → used as-is
	}
	for _, c := range cases {
		if got := nodeDialTimeout(c.in); got != c.want {
			t.Errorf("nodeDialTimeout(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNodeRequestIDUnique(t *testing.T) {
	a := nodeRequestID("ping")
	b := nodeRequestID("ping")
	if a == b {
		t.Errorf("request ids not unique: %q == %q", a, b)
	}
	if !strings.HasPrefix(a, "yc-ping-") {
		t.Errorf("request id %q missing expected prefix", a)
	}
}

func TestResolveNodeCaptureHookResponsive(t *testing.T) {
	fh := startFakeHook(t, 34572, "tok")
	withNodeConfig(t, "hook", fh.dir)

	ctx := ResolveNodeCapture(fh.pid)
	if !ctx.HookAvailable() {
		t.Fatalf("expected hook available, HookErr=%v", ctx.HookErr)
	}
	if ctx.Client == nil {
		t.Errorf("expected a non-nil hook client")
	}
	if ctx.Hook == nil || ctx.Hook.HookVersion == "" {
		t.Errorf("expected ping identity, got %+v", ctx.Hook)
	}
}

func TestResolveNodeCaptureDeadSocket(t *testing.T) {
	dir := t.TempDir()
	withNodeConfig(t, "hook", dir)
	// Registration points at a socket path with no listener → dial fails fast.
	pid := 34573
	reg := fmt.Sprintf(`{"pid":%d,"pipePath":%q,"nodeVersion":"v18","platform":"linux","startedAt":"x"}`, pid, filepath.Join(dir, "dead.sock"))
	os.WriteFile(nodeRegistrationPath(dir, pid), []byte(reg), 0o600)
	os.WriteFile(filepath.Join(dir, nodeTokenFileName), []byte("t"), 0o600)
	InvalidateNodeToken()

	ctx := ResolveNodeCapture(pid)
	if ctx.HookAvailable() {
		t.Fatalf("expected dead socket to be treated as no hook")
	}
	if ctx.HookErr == nil {
		t.Errorf("expected a HookErr for the dead socket")
	}
}
