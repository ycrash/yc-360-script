package config

import "testing"

func TestIsValidAppRuntimeNodejs(t *testing.T) {
	for _, v := range []string{"nodejs", "NodeJS", " nodejs ", "java", "dotnet", ""} {
		if !IsValidAppRuntime(v) {
			t.Errorf("IsValidAppRuntime(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"node", "python", "go", "dotnet-core"} {
		if IsValidAppRuntime(v) {
			t.Errorf("IsValidAppRuntime(%q) = true, want false", v)
		}
	}
}

func TestNodejsDefaults(t *testing.T) {
	c := defaultConfig()
	if c.NodejsCaptureMode != "hook" {
		t.Errorf("default NodejsCaptureMode = %q, want hook", c.NodejsCaptureMode)
	}
	if c.NodejsReportSignal != "SIGUSR2" {
		t.Errorf("default NodejsReportSignal = %q, want SIGUSR2", c.NodejsReportSignal)
	}
	if c.NodejsGCCaptureDuration.Duration().Seconds() != 30 {
		t.Errorf("default NodejsGCCaptureDuration = %v, want 30s", c.NodejsGCCaptureDuration)
	}
	if c.NodejsCPUProfileDuration.Duration().Seconds() != 30 {
		t.Errorf("default NodejsCPUProfileDuration = %v, want 30s", c.NodejsCPUProfileDuration)
	}
	if c.NodejsDiagnosticWindow.Duration().Seconds() != 30 {
		t.Errorf("default NodejsDiagnosticWindow = %v, want 30s", c.NodejsDiagnosticWindow)
	}
}

func TestNodejsFlagParsing(t *testing.T) {
	saved := GlobalConfig
	t.Cleanup(func() { GlobalConfig = saved })
	GlobalConfig = defaultConfig()

	if err := ParseFlags([]string{"yc", "-nodejsCaptureMode=signal", "-nodejsCPUProfileDuration=45s", "-nodejsReportSignal=SIGQUIT"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if GlobalConfig.NodejsCaptureMode != "signal" {
		t.Errorf("NodejsCaptureMode = %q, want signal", GlobalConfig.NodejsCaptureMode)
	}
	if GlobalConfig.NodejsCPUProfileDuration.Duration().Seconds() != 45 {
		t.Errorf("NodejsCPUProfileDuration = %v, want 45s", GlobalConfig.NodejsCPUProfileDuration)
	}
	if GlobalConfig.NodejsReportSignal != "SIGQUIT" {
		t.Errorf("NodejsReportSignal = %q, want SIGQUIT", GlobalConfig.NodejsReportSignal)
	}
}

func TestGetConfiguredAppRuntimeNodejs(t *testing.T) {
	saved := GlobalConfig
	t.Cleanup(func() { GlobalConfig = saved })
	GlobalConfig.AppRuntime = "NodeJS"
	if got := GetConfiguredAppRuntime(); got != "nodejs" {
		t.Errorf("GetConfiguredAppRuntime() = %q, want nodejs", got)
	}
	// With an explicit override, GetAppRuntime returns it without process inspection.
	if got := GetAppRuntime(0); got != "nodejs" {
		t.Errorf("GetAppRuntime with override = %q, want nodejs", got)
	}
}
