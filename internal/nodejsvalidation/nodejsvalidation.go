// Package nodejsvalidation holds the pure Node.js flag/config validation logic.
package nodejsvalidation

import (
	"errors"
	"runtime"
	"strings"

	"yc-agent/internal/capture"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// ErrInvalid indicates a Node.js flag/config value that must stop startup.
var ErrInvalid = errors.New("nodejs validation: invalid argument")

// Validate validates Node.js capture-mode and signal-related flags.
func Validate(appRuntime string) error {
	mode := strings.ToLower(strings.TrimSpace(config.GlobalConfig.NodejsCaptureMode))

	// Only enforce when the runtime is (or could be) Node. When appRuntime is
	// unset (auto-detect), we still validate a non-default capture mode.
	if appRuntime != "nodejs" && mode == "hook" {
		return nil
	}

	switch mode {
	case "", "hook":
		// hook mode: no signal constraints apply.
	case "signal":
		// Signal mode is POSIX-only.
		if runtime.GOOS == "windows" {
			logger.Log("-nodejsCaptureMode=signal is not supported on Windows. Use hook mode instead.")
			return ErrInvalid
		}
		// The report signal must be a valid, non-SIGUSR1 signal.
		if err := ValidateSignal("-nodejsReportSignal", config.GlobalConfig.NodejsReportSignal); err != nil {
			return err
		}
		if strings.TrimSpace(config.GlobalConfig.NodejsHeapdumpSignal) != "" {
			if err := ValidateSignal("-nodejsHeapdumpSignal", config.GlobalConfig.NodejsHeapdumpSignal); err != nil {
				return err
			}
		}
	default:
		logger.Log("invalid -nodejsCaptureMode %q. Expected one of: hook, signal", config.GlobalConfig.NodejsCaptureMode)
		return ErrInvalid
	}

	return nil
}

// ValidateSignal refuses SIGUSR1 (which unconditionally activates Node's
// inspector — a security exposure) and rejects clearly malformed signal names.
func ValidateSignal(flagName, value string) error {
	if err := capture.ValidateNodeSignalName(value); err != nil {
		logger.Log("%s has invalid value %q: %s", flagName, value, err)
		return ErrInvalid
	}
	return nil
}
