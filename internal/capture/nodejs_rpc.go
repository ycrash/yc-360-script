package capture

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// This file wraps the hook RPCs as typed Go methods on NodeHookClient. The wire
// details (framing, token auth, id-matched request/response) live in
// nodejs_ipc.go; here we deal with params, result decoding and per-RPC timeouts.
//
// ping is the liveness/identity RPC the discovery handshake uses to confirm a
// hook socket is responsive. The capture RPCs — dumpProcessOverview,
// dumpHeapSummary, dumpGC, dumpCPUProfile and the Diagnostic Report page
// captures — follow it; the asynchronous ones derive their own longer timeouts
// from the requested window so the read deadline outlasts the hook's delayed
// response.

const (
	// nodeSyncTimeout bounds the synchronous RPCs (ping, dumpProcessOverview,
	// dumpHeapSummary, dumpModuleInventory). These respond near-instantly, so the
	// timeout is generous.
	nodeSyncTimeout = 60 * time.Second
	// nodeAsyncMargin is added on top of a requested window for the async RPCs
	// (dumpGC, dumpCPUProfile and the windowed Diagnostic Report captures) so the
	// read deadline outlasts the hook's post-window response.
	nodeAsyncMargin = 20 * time.Second

	// nodeMaxDumpGCMs mirrors the hook's own dumpGC cap; the agent validates
	// client-side too, to fail fast.
	nodeMaxDumpGCMs = 60000

	// nodeMinWindowSeconds/nodeMaxWindowSeconds bound every windowed RPC's
	// windowSeconds, matching the hook's shared validateWindowSeconds. 0 is
	// rejected (not remapped to a default), exactly as the hook does.
	nodeMinWindowSeconds = 1
	nodeMaxWindowSeconds = 300

	nodeMinCPUProfileSeconds = nodeMinWindowSeconds
	nodeMaxCPUProfileSeconds = nodeMaxWindowSeconds
)

// NodePingResult is the identity/liveness payload returned by ping.
type NodePingResult struct {
	PID         int     `json:"pid"`
	NodeVersion string  `json:"nodeVersion"`
	Platform    string  `json:"platform"`
	UptimeSec   float64 `json:"uptimeSec"`
	HookVersion string  `json:"hookVersion"`
}

// Ping confirms the hook is responsive and returns its identity. It is called
// during discovery (ResolveNodeCapture) to distinguish a live hook from a stale
// registration file left behind by a crashed process or PID reuse.
func (c *NodeHookClient) Ping() (*NodePingResult, error) {
	resp, err := c.call("ping", nil, nodeSyncTimeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node ping failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodePingResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node ping result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// DumpProcessOverview requests a Node Diagnostic Report, written to outPath.
func (c *NodeHookClient) DumpProcessOverview(outPath string) error {
	resp, err := c.call("dumpProcessOverview", map[string]any{"outPath": outPath}, nodeSyncTimeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("node dumpProcessOverview failed pid=%d: %s", c.PID, resp.Error)
	}
	return nil
}

// DumpHeapSummary requests a lightweight heap overview + per-space histogram
// written to outPath.
func (c *NodeHookClient) DumpHeapSummary(outPath string) error {
	resp, err := c.call("dumpHeapSummary", map[string]any{"outPath": outPath}, nodeSyncTimeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("node dumpHeapSummary failed pid=%d: %s", c.PID, resp.Error)
	}
	return nil
}

// NodeGCResult is the (post-window) response of dumpGC.
type NodeGCResult struct {
	StartedAt     string  `json:"startedAt"`
	EndedAt       string  `json:"endedAt"`
	DurationMs    int64   `json:"durationMs"`
	ToggledOn     bool    `json:"toggledOn"`
	ToggledOff    bool    `json:"toggledOff"`
	ToggleError   *string `json:"toggleError"`
	UntoggleError *string `json:"untoggleError"`
	Note          string  `json:"note"`
}

// DumpGC toggles V8 --trace_gc on the target for durationMs, resolving only
// after the window elapses. GC trace lines land on the target's own stdout fd
// (not a hook-controlled file), so the caller must read that file for the
// [startedAt, endedAt] window separately.
func (c *NodeHookClient) DumpGC(durationMs int) (*NodeGCResult, error) {
	if durationMs <= 0 || durationMs > nodeMaxDumpGCMs {
		return nil, fmt.Errorf("dumpGC durationMs must be between 1 and %d, got %d", nodeMaxDumpGCMs, durationMs)
	}
	timeout := time.Duration(durationMs)*time.Millisecond + nodeAsyncMargin
	resp, err := c.call("dumpGC", map[string]any{"durationMs": durationMs}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpGC failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeGCResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpGC result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeCPUProfileResult is the (post-window) response of dumpCPUProfile.
type NodeCPUProfileResult struct {
	Path                    string  `json:"path"`
	TotalSamples            int     `json:"totalSamples"`
	WindowSeconds           float64 `json:"windowSeconds"`
	CaptureStartWallClockMs int64   `json:"captureStartWallClockMs"`
}

// DumpCPUProfile runs V8's sampling profiler for windowSeconds and writes the
// raw CpuProfile JSON to outPath. windowSeconds must be in [1, 300]; 0 is an
// error, not "use default" (matching the hook).
func (c *NodeHookClient) DumpCPUProfile(outPath string, windowSeconds int) (*NodeCPUProfileResult, error) {
	if windowSeconds < nodeMinCPUProfileSeconds || windowSeconds > nodeMaxCPUProfileSeconds {
		return nil, fmt.Errorf("dumpCPUProfile windowSeconds must be between %d and %d, got %d", nodeMinCPUProfileSeconds, nodeMaxCPUProfileSeconds, windowSeconds)
	}
	timeout := time.Duration(windowSeconds)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpCPUProfile", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpCPUProfile failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeCPUProfileResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpCPUProfile result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeWorkerCPUProfilesResult is the (post-window) response of dumpWorkerCPUProfiles.
type NodeWorkerCPUProfilesResult struct {
	Path               string  `json:"path"`
	WorkerCount        int     `json:"workerCount"`
	TotalWorkerCount   int     `json:"totalWorkerCount"`
	UnresponsiveCount  int     `json:"unresponsiveCount"`
	WindowSeconds      float64 `json:"windowSeconds"`
}

// DumpWorkerCPUProfiles profiles the hottest live worker_threads isolates for
// windowSeconds and writes {totalWorkerCount, skippedCount, unresponsiveCount,
// profiled:[{threadId, profile}]} to outPath. Async. The hook's hard timeout is
// windowSeconds+15s for non-yielding workers, so the client read deadline must
// include that margin.
//
// Overhead note: this is a bounded, on-demand/onlyCapture diagnostic — not M3.
// Each targeted worker runs V8's sampling CPU profiler for windowSeconds
// (same mechanism as DumpCPUProfile). Cap and hottest-first selection live in
// the hook so a large worker leak does not multiply profiler cost unbounded.
func (c *NodeHookClient) DumpWorkerCPUProfiles(outPath string, windowSeconds int) (*NodeWorkerCPUProfilesResult, error) {
	if windowSeconds < nodeMinCPUProfileSeconds || windowSeconds > nodeMaxCPUProfileSeconds {
		return nil, fmt.Errorf("dumpWorkerCPUProfiles windowSeconds must be between %d and %d, got %d", nodeMinCPUProfileSeconds, nodeMaxCPUProfileSeconds, windowSeconds)
	}
	// Hook finish() hardTimeout is (windowSeconds + 15)s; keep agent deadline above that.
	timeout := time.Duration(windowSeconds+15)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpWorkerCPUProfiles", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpWorkerCPUProfiles failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeWorkerCPUProfilesResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpWorkerCPUProfiles result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeReportValid reports whether the file at path exists, is non-empty and
// contains well-formed JSON.
func NodeReportValid(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	if len(data) == 0 {
		return false
	}
	return json.Valid(data)
}

// validateNodeWindow bounds a windowed-RPC windowSeconds client-side, matching
// the hook's shared validateWindowSeconds: [1, 300], with 0 rejected rather than
// remapped to a default (0 is JavaScript-falsy).
func validateNodeWindow(rpc string, windowSeconds int) error {
	if windowSeconds < nodeMinWindowSeconds || windowSeconds > nodeMaxWindowSeconds {
		return fmt.Errorf("%s windowSeconds must be between %d and %d, got %d", rpc, nodeMinWindowSeconds, nodeMaxWindowSeconds, windowSeconds)
	}
	return nil
}

// NodeEventLoopLagResult is the (post-window) response of dumpEventLoopLag.
type NodeEventLoopLagResult struct {
	Path          string  `json:"path"`
	SampleCount   int     `json:"sampleCount"`
	WindowSeconds float64 `json:"windowSeconds"`
}

// DumpEventLoopLag samples event-loop lag once/sec for windowSeconds, writing an
// ordered array of {timeLabel, count} to outPath. Async: resolves only
// after the window elapses.
func (c *NodeHookClient) DumpEventLoopLag(outPath string, windowSeconds int) (*NodeEventLoopLagResult, error) {
	if err := validateNodeWindow("dumpEventLoopLag", windowSeconds); err != nil {
		return nil, err
	}
	timeout := time.Duration(windowSeconds)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpEventLoopLag", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpEventLoopLag failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeEventLoopLagResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpEventLoopLag result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeUnhandledRejectionsResult is the (post-window) response of
// dumpUnhandledRejections.
type NodeUnhandledRejectionsResult struct {
	Path          string  `json:"path"`
	EventCount    int     `json:"eventCount"`
	TotalCount    int     `json:"totalCount"`
	Truncated     bool    `json:"truncated"`
	WindowSeconds float64 `json:"windowSeconds"`
}

// DumpUnhandledRejections buffers unhandledRejection events for windowSeconds,
// writing {reason, stackTrace, epochMs} per event to outPath. Async.
func (c *NodeHookClient) DumpUnhandledRejections(outPath string, windowSeconds int) (*NodeUnhandledRejectionsResult, error) {
	if err := validateNodeWindow("dumpUnhandledRejections", windowSeconds); err != nil {
		return nil, err
	}
	timeout := time.Duration(windowSeconds)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpUnhandledRejections", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpUnhandledRejections failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeUnhandledRejectionsResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpUnhandledRejections result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeModuleInventoryResult is the (synchronous) response of dumpModuleInventory.
type NodeModuleInventoryResult struct {
	Path         string `json:"path"`
	PackageCount int    `json:"packageCount"`
	FileCount    int    `json:"fileCount"`
}

// DumpModuleInventory walks require.cache and writes {name, version, fileCount}
// per resolved package to outPath. Synchronous — no window, no overlap guard,
// arrives immediately.
func (c *NodeHookClient) DumpModuleInventory(outPath string) (*NodeModuleInventoryResult, error) {
	resp, err := c.call("dumpModuleInventory", map[string]any{"outPath": outPath}, nodeSyncTimeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpModuleInventory failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeModuleInventoryResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpModuleInventory result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeHandleGrowthResult is the (post-window) response of dumpHandleGrowth.
type NodeHandleGrowthResult struct {
	Path            string  `json:"path"`
	SampleCount     int     `json:"sampleCount"`
	WindowSeconds   float64 `json:"windowSeconds"`
	IntervalSeconds float64 `json:"intervalSeconds"`
}

// DumpHandleGrowth samples active-resource type counts every intervalSeconds for
// windowSeconds, writing {statesCount, totalCount, epochMs} per sample to outPath.
// Async. intervalSeconds MUST be >= 1 and strictly less than windowSeconds.
func (c *NodeHookClient) DumpHandleGrowth(outPath string, windowSeconds, intervalSeconds int) (*NodeHandleGrowthResult, error) {
	if err := validateNodeWindow("dumpHandleGrowth", windowSeconds); err != nil {
		return nil, err
	}
	if intervalSeconds < 1 {
		return nil, fmt.Errorf("dumpHandleGrowth intervalSeconds must be >= 1, got %d", intervalSeconds)
	}
	if intervalSeconds >= windowSeconds {
		return nil, fmt.Errorf("dumpHandleGrowth intervalSeconds (%d) must be smaller than windowSeconds (%d)", intervalSeconds, windowSeconds)
	}
	timeout := time.Duration(windowSeconds)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpHandleGrowth", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds, "intervalSeconds": intervalSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpHandleGrowth failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeHandleGrowthResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpHandleGrowth result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}

// NodeGCStatsResult is the (post-window) response of dumpGCStats.
type NodeGCStatsResult struct {
	Path          string  `json:"path"`
	EventCount    int     `json:"eventCount"`
	WindowSeconds float64 `json:"windowSeconds"`
}

// DumpGCStats buffers structured per-GC-event {kind, durationMs, epochMs} via
// perf_hooks for windowSeconds, writing them to outPath. Async.
func (c *NodeHookClient) DumpGCStats(outPath string, windowSeconds int) (*NodeGCStatsResult, error) {
	if err := validateNodeWindow("dumpGCStats", windowSeconds); err != nil {
		return nil, err
	}
	timeout := time.Duration(windowSeconds)*time.Second + nodeAsyncMargin
	resp, err := c.call("dumpGCStats", map[string]any{"outPath": outPath, "windowSeconds": windowSeconds}, timeout)
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("node dumpGCStats failed pid=%d: %s", c.PID, resp.Error)
	}
	var r NodeGCStatsResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, fmt.Errorf("node dumpGCStats result decode failed pid=%d: %w", c.PID, err)
	}
	return &r, nil
}
