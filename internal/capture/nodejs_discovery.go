package capture

import (
	"fmt"
	"strings"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// Node capture modes.
const (
	NodeCaptureModeHook   = "hook"
	NodeCaptureModeSignal = "signal"
)

// NodeCaptureContext is the result of resolving how a specific Node.js PID can
// be captured.
type NodeCaptureContext struct {
	PID        int
	RuntimeDir string
	Mode       string
	Client     *NodeHookClient
	Hook       *NodePingResult
	HookErr    error
}

// HookAvailable reports whether a responsive hook client is ready to use.
func (ctx *NodeCaptureContext) HookAvailable() bool {
	return ctx != nil && ctx.Client != nil
}

// ResolveNodeCapture determines the capture mode and, in hook mode, whether the
// hook is present and responsive for pid.
func ResolveNodeCapture(pid int) *NodeCaptureContext {
	mode := NormalizeNodeCaptureMode(config.GlobalConfig.NodejsCaptureMode)
	runtimeDir := NodeRuntimeDir()

	ctx := &NodeCaptureContext{PID: pid, RuntimeDir: runtimeDir, Mode: mode}

	// Windows has no usable signal delivery
	if mode == NodeCaptureModeSignal {
		if IsWindows() {
			ctx.HookErr = fmt.Errorf("signal mode is not supported on Windows")
			logger.Log("WARNING: node signal mode requested for pid %d but is unsupported on Windows", pid)
		}
		return ctx
	}

	// Hook mode
	client, err := NewNodeHookClient(runtimeDir, pid)
	if err != nil {
		ctx.HookErr = err
		logNodeHookUnavailable(pid, runtimeDir, err)
		return ctx
	}

	// Registration present — confirm the socket/pipe is actually responsive
	ping, err := client.Ping()
	if err != nil {
		// Stale registration from a crashed/killed process, or PID reuse.
		ctx.HookErr = fmt.Errorf("hook registration present but not responsive: %w", err)
		logger.Log("WARNING: node hook registration for pid %d exists but the socket is unresponsive (treating as no hook): %s", pid, err)
		return ctx
	}

	ctx.Client = client
	ctx.Hook = ping
	logger.Log("node hook responsive for pid %d: hookVersion=%s nodeVersion=%s platform=%s", pid, ping.HookVersion, ping.NodeVersion, ping.Platform)
	return ctx
}

// NormalizeNodeCaptureMode lowercases/trims the configured capture mode and
// defaults an empty value to hook mode.
func NormalizeNodeCaptureMode(value string) string {
	m := strings.ToLower(strings.TrimSpace(value))
	if m == "" {
		return NodeCaptureModeHook
	}
	return m
}

func logNodeHookUnavailable(pid int, runtimeDir string, err error) {
	hookPath := strings.TrimSpace(config.GlobalConfig.NodejsHookPath)
	if hookPath == "" {
		hookPath = "/path/to/yc360-node-hook.js"
	}
	logger.Log("WARNING: no responsive yc-360 Node.js hook found for pid %d in %s (%s).", pid, runtimeDir, err)
	logger.Log(`To enable Node.js diagnostics capture for pid %d, choose ONE:
  1. Hook mode (recommended, all platforms): start the process with
       NODE_OPTIONS="--require=%s"
     and restart it, then re-run yc-360.
  2. Signal mode (POSIX only, no hook): start the process with the native
     Node flags (--report-on-signal[=SIGUSR2] and/or --trace-gc), then re-run
     yc-360 with -nodejsCaptureMode=signal.`, pid, hookPath)
}
