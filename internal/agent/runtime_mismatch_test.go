package agent

import (
	"testing"

	"yc-agent/internal/capture/runtime"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
)

func TestRunStopsOnAppRuntimeMismatch(t *testing.T) {
	originalConfig := config.GlobalConfig
	originalDetect := config.ProcessRuntimeDetector
	t.Cleanup(func() {
		config.GlobalConfig = originalConfig
		config.ProcessRuntimeDetector = originalDetect
	})
	logger.Init("", 0, 0, "info")

	// -appRuntime nodejs with a PID that is certainly a Java process: the run
	// must stop before FullCapture (which would start real captures).
	config.GlobalConfig = config.Config{Options: config.Options{
		Pid:         "4242",
		AppRuntime:  "nodejs",
		OnlyCapture: true,
	}}
	config.ProcessRuntimeDetector = func(int) (runtime.RuntimeType, string) {
		return runtime.RuntimeJava, "java.exe"
	}

	assert.Equal(t, ErrAppRuntimeMismatch, runOnDemandMode())
}
