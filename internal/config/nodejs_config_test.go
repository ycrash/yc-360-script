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

func TestNodejsSignalDefaults(t *testing.T) {
	c := defaultConfig()
	if c.NodejsCaptureMode != "hook" {
		t.Errorf("default NodejsCaptureMode = %q, want hook", c.NodejsCaptureMode)
	}
	if c.NodejsReportSignal != "SIGUSR2" {
		t.Errorf("default NodejsReportSignal = %q, want SIGUSR2", c.NodejsReportSignal)
	}
	if c.NodejsHeapdumpSignal != "SIGUSR2" {
		t.Errorf("default NodejsHeapdumpSignal = %q, want SIGUSR2", c.NodejsHeapdumpSignal)
	}
}

func TestNodejsSignalFlagParsing(t *testing.T) {
	saved := GlobalConfig
	t.Cleanup(func() { GlobalConfig = saved })
	GlobalConfig = defaultConfig()

	if err := ParseFlags([]string{"yc", "-nodejsCaptureMode=signal", "-nodejsReportSignal=SIGQUIT", "-nodejsHeapdumpSignal=SIGABRT"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	if GlobalConfig.NodejsCaptureMode != "signal" {
		t.Errorf("NodejsCaptureMode = %q, want signal", GlobalConfig.NodejsCaptureMode)
	}
	if GlobalConfig.NodejsReportSignal != "SIGQUIT" {
		t.Errorf("NodejsReportSignal = %q, want SIGQUIT", GlobalConfig.NodejsReportSignal)
	}
	if GlobalConfig.NodejsHeapdumpSignal != "SIGABRT" {
		t.Errorf("NodejsHeapdumpSignal = %q, want SIGABRT", GlobalConfig.NodejsHeapdumpSignal)
	}
}
