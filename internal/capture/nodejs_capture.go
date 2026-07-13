package capture

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

const (
	nodeDTProcessOverview = "nodepo"
	nodeDTEventLoopLag    = "nodeell"
	nodeDTUnhandledRejections = "nodeur"
	nodeDTModuleInventory     = "nodemi"
	nodeDTHandleGrowth        = "nodehg"
	nodeDTGCStats             = "nodegcs"
)

func nodeAbsOutPath(outDir, name string) (string, error) {
	if outDir == "" {
		outDir = "."
	}
	return filepath.Abs(filepath.Join(outDir, name))
}

// NodeProcessOverview captures the Diagnostic Report page's Process Overview
// artifact to nodeJsProcessOverview.out, uploaded under dt=nodepo.
type NodeProcessOverview struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeProcessOverview) Run() (Result, error) {
	if !IsProcessExists(t.Pid) {
		return Result{Msg: fmt.Sprintf("process %d does not exist", t.Pid), Ok: false}, nil
	}
	outPath, err := nodeAbsOutPath(t.OutDir, NodeProcessOverviewFileName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}

	if t.Ctx != nil && t.Ctx.Mode == NodeCaptureModeSignal {
		return t.runSignal(outPath)
	}
	return t.runHook(outPath)
}

func (t *NodeProcessOverview) runHook(outPath string) (Result, error) {
	if t.Ctx == nil || !t.Ctx.HookAvailable() {
		return Result{Msg: fmt.Sprintf("node process overview skipped for pid %d: hook not available", t.Pid), Ok: false}, nil
	}
	if err := prepareNodeHookOutPath(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		err := t.Ctx.Client.DumpProcessOverview(outPath)
		if err != nil {
			// The RPC itself failed. If the process died, surface that distinctly.
			if !IsProcessExists(t.Pid) {
				return Result{Msg: fmt.Sprintf("node process %d died during process overview capture", t.Pid), Ok: false}, nil
			}
			if attempt < maxAttempts {
				logger.Log("node dumpProcessOverview pid=%d attempt %d failed (%s); retrying", t.Pid, attempt, err)
				time.Sleep(750 * time.Millisecond)
				continue
			}
			return Result{Msg: err.Error(), Ok: false}, nil
		}

		if NodeReportValid(outPath) {
			return t.upload(outPath), nil
		}

		// Output is present but not well-formed JSON — the classic crash signature.
		if !IsProcessExists(t.Pid) {
			return Result{Msg: fmt.Sprintf("node process %d died during process overview capture (truncated/invalid report)", t.Pid), Ok: false}, nil
		}
		if attempt < maxAttempts {
			logger.Log("node dumpProcessOverview pid=%d produced invalid JSON on attempt %d; retrying after short delay", t.Pid, attempt)
			time.Sleep(750 * time.Millisecond)
			continue
		}
		return Result{Msg: fmt.Sprintf("node process overview for pid %d is not well-formed JSON after %d attempts", t.Pid, maxAttempts), Ok: false}, nil
	}

	return Result{Msg: "unreachable", Ok: false}, nil
}

func (t *NodeProcessOverview) runSignal(outPath string) (Result, error) {
	timeout := nodeSignalCaptureTimeout()
	reportPath, err := NodeSignalReportCapture(t.Pid, config.GlobalConfig.NodejsReportSignal, timeout)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}
	if err := nodeMoveFile(reportPath, outPath); err != nil {
		return Result{Msg: fmt.Sprintf("failed moving report %s to %s: %s", reportPath, outPath, err), Ok: false}, nil
	}
	if !NodeReportValid(outPath) {
		return Result{Msg: fmt.Sprintf("node signal-mode report for pid %d is not well-formed JSON", t.Pid), Ok: false}, nil
	}
	if err := reshapeNodeReportToProcessOverview(outPath); err != nil {
		return Result{Msg: fmt.Sprintf("failed reshaping signal-mode report for pid %d: %s", t.Pid, err), Ok: false}, nil
	}
	return t.upload(outPath), nil
}

func (t *NodeProcessOverview) upload(outPath string) Result {
	file, err := os.Open(outPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed opening %s: %s", outPath, err), Ok: false}
	}
	defer file.Close()
	msg, ok := PostData(t.Endpoint(), nodeDTProcessOverview, file)
	return Result{Msg: msg, Ok: ok}
}

// ---------------------------------------------------------------------------
// Heap summary (heap substitute)
// ---------------------------------------------------------------------------

// NodeHeapSummary captures a lightweight heap overview + per-space histogram to
// hdsub.out. Hook-only: signal mode has no heap-summary equivalent.
type NodeHeapSummary struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeHeapSummary) Run() (Result, error) {
	if t.Ctx != nil && t.Ctx.Mode == NodeCaptureModeSignal {
		return Result{Msg: "node heap summary is unavailable in signal mode (no native-flag equivalent)", Ok: false}, nil
	}
	if t.Ctx == nil || !t.Ctx.HookAvailable() {
		return Result{Msg: fmt.Sprintf("node heap summary skipped for pid %d: hook not available", t.Pid), Ok: false}, nil
	}
	if !IsProcessExists(t.Pid) {
		return Result{Msg: fmt.Sprintf("process %d does not exist", t.Pid), Ok: false}, nil
	}

	outPath, err := nodeAbsOutPath(t.OutDir, NodeHeapSummaryName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	if err := prepareNodeHookOutPath(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}
	if err := t.Ctx.Client.DumpHeapSummary(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}

	file, err := os.Open(outPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed opening %s: %s", outPath, err), Ok: false}, nil
	}
	defer file.Close()
	msg, ok := PostData(t.Endpoint(), "hdsub", file)
	return Result{Msg: msg, Ok: ok}, nil
}

// ---------------------------------------------------------------------------
// CPU profile
// ---------------------------------------------------------------------------

// NodeCPUProfile captures a V8 CPU profile to cpuprofile.out. Hook-only, opt-in
// (via -nodejsCPUProfile), and asynchronous over its window.
type NodeCPUProfile struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeCPUProfile) Run() (Result, error) {
	if t.Ctx == nil || !t.Ctx.HookAvailable() {
		return Result{Msg: fmt.Sprintf("node cpu profile skipped for pid %d: hook not available", t.Pid), Ok: false}, nil
	}
	if !IsProcessExists(t.Pid) {
		return Result{Msg: fmt.Sprintf("process %d does not exist", t.Pid), Ok: false}, nil
	}

	outPath, err := nodeAbsOutPath(t.OutDir, NodeCPUProfileFileName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	if err := prepareNodeHookOutPath(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}

	windowSeconds := nodeCPUProfileWindowSeconds()
	if _, err := t.Ctx.Client.DumpCPUProfile(outPath, windowSeconds); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}

	file, err := os.Open(outPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed opening %s: %s", outPath, err), Ok: false}, nil
	}
	defer file.Close()
	msg, ok := PostData(t.Endpoint(), "cpuprofile", file)
	return Result{Msg: msg, Ok: ok}, nil
}

func nodeDiagnosticCapture(endpoint string, pid int, ctx *NodeCaptureContext, outDir, fileName, label, dt string, doRPC func(outPath string) error) (Result, error) {
	if ctx != nil && ctx.Mode == NodeCaptureModeSignal {
		return Result{Msg: fmt.Sprintf("node %s is unavailable in signal mode (hook-only)", label), Ok: false}, nil
	}
	if ctx == nil || !ctx.HookAvailable() {
		return Result{Msg: fmt.Sprintf("node %s skipped for pid %d: hook not available", label, pid), Ok: false}, nil
	}
	if !IsProcessExists(pid) {
		return Result{Msg: fmt.Sprintf("process %d does not exist", pid), Ok: false}, nil
	}

	outPath, err := nodeAbsOutPath(outDir, fileName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	if err := prepareNodeHookOutPath(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}
	if err := doRPC(outPath); err != nil {
		return Result{Msg: err.Error(), Ok: false}, nil
	}

	file, err := os.Open(outPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed opening %s: %s", outPath, err), Ok: false}, nil
	}
	defer file.Close()
	msg, ok := PostData(endpoint, dt, file)
	return Result{Msg: msg, Ok: ok}, nil
}

// NodeEventLoopLag captures event-loop lag samples to eventlooplag.out.
type NodeEventLoopLag struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeEventLoopLag) Run() (Result, error) {
	window := nodeDiagnosticWindowSeconds()
	return nodeDiagnosticCapture(t.Endpoint(), t.Pid, t.Ctx, t.OutDir, NodeEventLoopLagFileName, "event loop lag", nodeDTEventLoopLag, func(outPath string) error {
		_, err := t.Ctx.Client.DumpEventLoopLag(outPath, window)
		return err
	})
}

// NodeUnhandledRejections captures unhandled promise rejections to rejections.out.
type NodeUnhandledRejections struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeUnhandledRejections) Run() (Result, error) {
	window := nodeDiagnosticWindowSeconds()
	return nodeDiagnosticCapture(t.Endpoint(), t.Pid, t.Ctx, t.OutDir, NodeUnhandledRejectionsFileName, "unhandled rejections", nodeDTUnhandledRejections, func(outPath string) error {
		res, err := t.Ctx.Client.DumpUnhandledRejections(outPath, window)
		if err == nil && res != nil && res.Truncated {
			logger.Log("node unhandled rejections pid=%d: captured %d of %d events (truncated at hook cap)", t.Pid, res.EventCount, res.TotalCount)
		}
		return err
	})
}

// NodeModuleInventory captures the loaded-package inventory to modules.out.
// Synchronous — no capture window.
type NodeModuleInventory struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeModuleInventory) Run() (Result, error) {
	return nodeDiagnosticCapture(t.Endpoint(), t.Pid, t.Ctx, t.OutDir, NodeModuleInventoryFileName, "module inventory", nodeDTModuleInventory, func(outPath string) error {
		_, err := t.Ctx.Client.DumpModuleInventory(outPath)
		return err
	})
}

// NodeHandleGrowth captures active handle/request growth samples to
// handlegrowth.out.
type NodeHandleGrowth struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeHandleGrowth) Run() (Result, error) {
	window := nodeDiagnosticWindowSeconds()
	interval := nodeHandleGrowthIntervalSeconds(window)
	return nodeDiagnosticCapture(t.Endpoint(), t.Pid, t.Ctx, t.OutDir, NodeHandleGrowthFileName, "handle growth", nodeDTHandleGrowth, func(outPath string) error {
		_, err := t.Ctx.Client.DumpHandleGrowth(outPath, window, interval)
		return err
	})
}

// NodeGCStats captures structured perf_hooks GC events to gcstats.out. Provided
// for completeness but not yet wired into the on-demand/M3 flow.
type NodeGCStats struct {
	Capture
	Pid    int
	Ctx    *NodeCaptureContext
	OutDir string
}

func (t *NodeGCStats) Run() (Result, error) {
	window := nodeDiagnosticWindowSeconds()
	return nodeDiagnosticCapture(t.Endpoint(), t.Pid, t.Ctx, t.OutDir, NodeGCStatsFileName, "gc stats", nodeDTGCStats, func(outPath string) error {
		_, err := t.Ctx.Client.DumpGCStats(outPath, window)
		return err
	})
}

// ---------------------------------------------------------------------------
// GC log (continuous split, or on-demand dumpGC fallback)
// ---------------------------------------------------------------------------

// NodeGC captures the GC log.
type NodeGC struct {
	Capture
	Pid     int
	Ctx     *NodeCaptureContext
	OutDir  string
	Tracker *NodeGCTracker // non-nil ⇒ M3 incremental delta reads
	M3      bool           // M3 mode: continuous only, no dumpGC fallback
}

func (t *NodeGC) Run() (Result, error) {
	gcOutPath, err := nodeAbsOutPath(t.OutDir, NodeGCLogFileName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	appOutPath, err := nodeAbsOutPath(t.OutDir, NodeAppLogFileName)
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}

	stdoutPath, stdoutErr := ResolveNodeStdoutFile(t.Pid)
	hasFlag := NodeHasTraceGCFlag(t.Pid)
	continuous := hasFlag && stdoutErr == nil && stdoutPath != ""

	if continuous {
		// Continuous mode reads the WHOLE stdout file (on-demand) or the delta
		// since last cycle (M3) and splits it into gc.log + nodeapp.log.
		if t.Tracker != nil {
			if fi, statErr := os.Stat(stdoutPath); statErr == nil {
				if t.Tracker.InitIfAbsent(t.Pid, stdoutPath, fi.Size()) {
					return Result{Msg: fmt.Sprintf("node gc: began M3 monitoring for pid %d at offset %d; GC/app delta uploads from the next cycle", t.Pid, fi.Size()), Ok: true}, nil
				}
			}
		}

		startOffset := int64(0)
		if t.Tracker != nil {
			startOffset = t.Tracker.Offset(t.Pid, stdoutPath)
		}
		newOffset, result := t.splitToFiles(stdoutPath, startOffset, gcOutPath, appOutPath, true)
		if t.Tracker != nil {
			t.Tracker.SetOffset(t.Pid, stdoutPath, newOffset)
		}
		return result, nil
	}

	// On-demand hook fallback: toggle --trace_gc for a bounded window, then read
	// the delta from the target's stdout file.
	if !t.M3 && t.Ctx != nil && t.Ctx.HookAvailable() {
		if stdoutErr != nil || stdoutPath == "" {
			return Result{Msg: fmt.Sprintf("node gc log unavailable for pid %d: --trace-gc not set and stdout is not a readable file (%v)", t.Pid, stdoutErr), Ok: false}, nil
		}
		return t.captureViaDumpGC(stdoutPath, gcOutPath), nil
	}

	reason := "continuous --trace-gc not detected"
	if stdoutErr != nil {
		reason = stdoutErr.Error()
	} else if !hasFlag {
		reason = "process was not started with --trace-gc"
	}
	return Result{Msg: fmt.Sprintf("node gc log not captured for pid %d: %s", t.Pid, reason), Ok: false}, nil
}

// captureViaDumpGC records the stdout file size, toggles GC tracing for a
// window via the hook, then extracts GC lines from the newly-appended slice.
func (t *NodeGC) captureViaDumpGC(stdoutPath, gcOutPath string) Result {
	var startSize int64
	if fi, err := os.Stat(stdoutPath); err == nil {
		startSize = fi.Size()
	}

	durationMs := int(config.GlobalConfig.NodejsGCCaptureDuration.Duration() / time.Millisecond)
	if durationMs <= 0 {
		durationMs = 30000
	}
	if durationMs > nodeMaxDumpGCMs {
		durationMs = nodeMaxDumpGCMs
	}

	logger.Log("node gc: pid %d not started with --trace-gc; using bounded dumpGC window of %dms", t.Pid, durationMs)
	if _, err := t.Ctx.Client.DumpGC(durationMs); err != nil {
		return Result{Msg: fmt.Sprintf("node dumpGC failed for pid %d: %s", t.Pid, err), Ok: false}
	}

	_, result := t.splitToFiles(stdoutPath, startSize, gcOutPath, "", false)
	return result
}

func (t *NodeGC) splitToFiles(stdoutPath string, startOffset int64, gcOutPath, appOutPath string, writeAppLog bool) (int64, Result) {
	gcFile, err := os.Create(gcOutPath)
	if err != nil {
		return startOffset, Result{Msg: fmt.Sprintf("failed creating %s: %s", gcOutPath, err), Ok: false}
	}

	var otherOut io.Writer
	var appFile *os.File
	if writeAppLog {
		appFile, err = os.Create(appOutPath)
		if err != nil {
			gcFile.Close()
			return startOffset, Result{Msg: fmt.Sprintf("failed creating %s: %s", appOutPath, err), Ok: false}
		}
		otherOut = appFile
	} else {
		otherOut = io.Discard
	}

	newOffset, _, otherLines, splitErr := SplitNodeGCFrom(stdoutPath, startOffset, gcFile, otherOut)

	_ = gcFile.Sync()
	closeErr := gcFile.Close()
	if appFile != nil {
		_ = appFile.Sync()
		if e := appFile.Close(); closeErr == nil {
			closeErr = e
		}
	}
	if splitErr != nil {
		return startOffset, Result{Msg: fmt.Sprintf("failed reading node gc log %s: %s", stdoutPath, splitErr), Ok: false}
	}
	if closeErr != nil {
		return newOffset, Result{Msg: fmt.Sprintf("failed writing node gc artifacts: %s", closeErr), Ok: false}
	}

	result := t.uploadGCFile(gcOutPath)

	if writeAppLog {
		if otherLines > 0 {
			t.uploadAppFile(appOutPath)
		} else {
			// Nothing but GC lines; don't leave an empty file in the zip.
			_ = os.Remove(appOutPath)
		}
	}
	return newOffset, result
}

func (t *NodeGC) uploadGCFile(gcOutPath string) Result {
	gcFile, err := os.Open(gcOutPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed opening %s: %s", gcOutPath, err), Ok: false}
	}
	defer gcFile.Close()

	if fi, statErr := gcFile.Stat(); statErr == nil && fi.Size() == 0 {
		return Result{Msg: fmt.Sprintf("no GC lines captured for pid %d (no GC activity in this window — expected, not a failure)", t.Pid), Ok: true}
	}

	msg, ok := PostData(t.Endpoint(), "gc", gcFile)
	return Result{Msg: msg, Ok: ok}
}

func (t *NodeGC) uploadAppFile(appOutPath string) {
	appFile, err := os.Open(appOutPath)
	if err != nil {
		return
	}
	defer appFile.Close()
	msg, ok := PostCustomData(t.Endpoint(), "dt=applog&logName="+NodeAppLogFileName, appFile)
	logger.Log("node gc: uploaded split-off non-GC output as %s (ok=%t): %s", NodeAppLogFileName, ok, msg)
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func nodeSignalCaptureTimeout() time.Duration {
	if d := config.GlobalConfig.CmdTimeout.Duration(); d > 0 {
		return d
	}
	return 60 * time.Second
}

func nodeCPUProfileWindowSeconds() int {
	seconds := int(config.GlobalConfig.NodejsCPUProfileDuration.Duration().Seconds())
	if seconds < nodeMinCPUProfileSeconds {
		seconds = 30
	}
	if seconds > nodeMaxCPUProfileSeconds {
		seconds = nodeMaxCPUProfileSeconds
	}
	return seconds
}

func nodeDiagnosticWindowSeconds() int {
	seconds := int(config.GlobalConfig.NodejsDiagnosticWindow.Duration().Seconds())
	if seconds < nodeMinWindowSeconds {
		seconds = 30
	}
	if seconds > nodeMaxWindowSeconds {
		seconds = nodeMaxWindowSeconds
	}
	return seconds
}

func nodeHandleGrowthIntervalSeconds(window int) int {
	interval := 5
	if interval >= window {
		interval = max(window/2, 1)
	}
	return interval
}

func reshapeNodeReportToProcessOverview(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var report map[string]any
	if err := json.Unmarshal(data, &report); err != nil {
		return err
	}

	// callStack: the flat frame-string array from javascriptStack.stack, or null
	// if the report shape is unexpected.
	var callStack any
	if js, ok := report["javascriptStack"].(map[string]any); ok {
		if stack, ok := js["stack"].([]any); ok {
			callStack = stack
		}
	}
	report["callStack"] = callStack

	delete(report, "javascriptStack")
	delete(report, "nativeStack")
	delete(report, "sharedObjects")
	delete(report, "workers")
	delete(report, "environmentVariables")

	out, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

func nodeMoveFile(src, dst string) error {
	if src == dst {
		return nil
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

func prepareNodeHookOutPath(outPath string) error {
	dir := filepath.Dir(outPath)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("failed creating node hook output directory %s: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		logger.Log("WARNING: failed setting node hook output directory %s to 0777: %s; hook writes may fail if the target process runs as another user", dir, err)
	}
	if err := os.Remove(outPath); err != nil && !os.IsNotExist(err) {
		logger.Log("WARNING: failed removing stale node hook artifact %s: %s; hook overwrite may fail if file ownership differs", outPath, err)
	}
	return nil
}
