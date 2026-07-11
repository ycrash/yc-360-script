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
	Reg        *NodeRegistration
	HookErr    error
}

// HookAvailable reports whether a usable hook was found for the PID.
func (ctx *NodeCaptureContext) HookAvailable() bool {
	return ctx != nil && ctx.Reg != nil
}

// ResolveNodeCapture determines the capture mode and, in hook mode, whether a
// hook registration is present for pid.
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
	reg, err := ReadNodeRegistration(runtimeDir, pid)
	if err != nil {
		ctx.HookErr = err
		logNodeHookUnavailable(pid, runtimeDir, err)
		return ctx
	}

	ctx.Reg = reg

	logger.Log("node hook registration found for pid %d: nodeVersion=%s platform=%s (liveness check deferred to hook IPC client)", pid, reg.NodeVersion, reg.Platform)
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
	logger.Log("WARNING: no yc-360 Node.js hook registration found for pid %d in %s (%s).", pid, runtimeDir, err)
	logger.Log(`To enable Node.js diagnostics capture for pid %d, choose ONE:
  1. Hook mode (recommended, all platforms): start the process with
       NODE_OPTIONS="--require=%s"
     and restart it, then re-run yc-360.
  2. Signal mode (POSIX only, no hook): start the process with the native
     Node flags (--report-on-signal[=SIGUSR2] and/or --trace-gc), then re-run
     yc-360 with -nodejsCaptureMode=signal.`, pid, hookPath)
}
