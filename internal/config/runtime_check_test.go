package config

import (
	"testing"

	"yc-agent/internal/capture/runtime"

	"github.com/stretchr/testify/assert"
)

func TestCheckAppRuntimeMatches(t *testing.T) {
	originalConfig := GlobalConfig
	originalDetect := ProcessRuntimeDetector
	t.Cleanup(func() {
		GlobalConfig = originalConfig
		ProcessRuntimeDetector = originalDetect
	})

	cases := []struct {
		name       string
		configured string
		actual     runtime.RuntimeType
		exe        string
		wantErr    string // "" means no error
	}{
		{"nodejs flag on a java process", "nodejs", runtime.RuntimeJava, "java.exe",
			"PID 42 is a Java process (java.exe), but -appRuntime nodejs was passed. Pass -appRuntime java, or remove -appRuntime and the yc-360 script will detect the runtime automatically."},
		{"java flag on a node process", "java", runtime.RuntimeNodeJS, "node", "PID 42 is a Node.js process (node), but -appRuntime java"},
		{"dotnet detected by modules, no exe name", "java", runtime.RuntimeDotNet, "", "PID 42 is a .NET process, but -appRuntime java"},
		{"flag matches the process", "java", runtime.RuntimeJava, "java.exe", ""},
		{"flag case and spaces are normalized", " NodeJS ", runtime.RuntimeNodeJS, "node.exe", ""},
		{"runtime can't be determined", "nodejs", runtime.RuntimeUnknown, "tomcat9.exe", ""},
		{"no -appRuntime set", "", runtime.RuntimeJava, "java.exe", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			GlobalConfig.AppRuntime = c.configured
			ProcessRuntimeDetector = func(int) (runtime.RuntimeType, string) { return c.actual, c.exe }

			err := CheckAppRuntimeMatches(42)
			if c.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), c.wantErr)
			}
		})
	}
}
