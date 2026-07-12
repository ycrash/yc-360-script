package nodejsvalidation

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"yc-agent/internal/config"
)

func TestValidateSignal(t *testing.T) {
	if err := ValidateSignal("-x", "SIGUSR2"); err != nil {
		t.Errorf("SIGUSR2 should be accepted, got %v", err)
	}
	for _, bad := range []string{"SIGUSR1", "usr1", "", "SIGTERM"} {
		err := ValidateSignal("-x", bad)
		if err == nil {
			t.Errorf("%q must be refused", bad)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q should map to ErrInvalid, got %v", bad, err)
		}
	}
}

func TestValidateCaptureMode(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	// hook mode: always fine.
	config.GlobalConfig.NodejsCaptureMode = "hook"
	if err := Validate("nodejs"); err != nil {
		t.Errorf("hook mode should validate, got %v", err)
	}

	// bogus mode when runtime is nodejs.
	config.GlobalConfig.NodejsCaptureMode = "bogus"
	if err := Validate("nodejs"); err == nil {
		t.Errorf("bogus capture mode must be rejected")
	}

	// signal mode with SIGUSR1 must be refused.
	config.GlobalConfig.NodejsCaptureMode = "signal"
	config.GlobalConfig.NodejsReportSignal = "SIGUSR1"
	if err := Validate("nodejs"); err == nil {
		t.Errorf("signal mode with SIGUSR1 must be rejected")
	}

	// signal mode with SIGUSR2: valid on POSIX, refused on Windows.
	config.GlobalConfig.NodejsReportSignal = "SIGUSR2"
	err := Validate("nodejs")
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Errorf("signal mode must be refused on Windows")
		}
	} else if err != nil {
		t.Errorf("signal mode with SIGUSR2 should validate on POSIX, got %v", err)
	}
}

func TestValidateInvalidCaptureModeRejected(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	config.GlobalConfig.NodejsCaptureMode = "singal"
	if err := Validate("nodejs"); err == nil {
		t.Errorf("invalid -nodejsCaptureMode \"singal\" must be rejected for a nodejs runtime")
	}
	if err := Validate(""); err == nil {
		t.Errorf("invalid -nodejsCaptureMode must be rejected even when the runtime is auto-detect")
	}
}

func TestValidateHeapdumpSignal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal mode is POSIX-only")
	}
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	config.GlobalConfig.NodejsCaptureMode = "signal"
	config.GlobalConfig.NodejsReportSignal = "SIGUSR2" // valid, so validation reaches the heapdump signal

	config.GlobalConfig.NodejsHeapdumpSignal = "SIGUSR1"
	if err := Validate("nodejs"); err == nil {
		t.Errorf("signal mode with a SIGUSR1 heapdump signal must be rejected")
	}

	config.GlobalConfig.NodejsHeapdumpSignal = "SIGUSR2"
	if err := Validate("nodejs"); err != nil {
		t.Errorf("signal mode with a valid heapdump signal should pass, got %v", err)
	}
}

func TestValidateDurationsNotClamped(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	config.GlobalConfig.NodejsCaptureMode = "hook"
	config.GlobalConfig.NodejsGCCaptureDuration = config.Duration(120 * time.Second)
	config.GlobalConfig.NodejsCPUProfileDuration = config.Duration(9999 * time.Second)
	config.GlobalConfig.NodejsDiagnosticWindow = config.Duration(9999 * time.Second)

	if err := Validate("nodejs"); err != nil {
		t.Errorf("validation must accept out-of-range Node durations (clamped later at capture), got %v", err)
	}
}
