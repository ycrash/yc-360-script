package ondemand

import (
	"testing"

	"yc-agent/internal/capture/runtime"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
)

func TestProcessPidsSkipsAppRuntimeMismatch(t *testing.T) {
	originalConfig := config.GlobalConfig
	originalDetect := config.ProcessRuntimeDetector
	t.Cleanup(func() {
		config.GlobalConfig = originalConfig
		config.ProcessRuntimeDetector = originalDetect
	})
	logger.Init("", 0, 0, "info")

	config.GlobalConfig.AppRuntime = "nodejs"
	config.ProcessRuntimeDetector = func(int) (runtime.RuntimeType, string) {
		return runtime.RuntimeJava, "java.exe"
	}

	// The PID is skipped (no capture is started) and the reason is returned,
	// so an API caller gets an error instead of an empty success.
	rUrls, err := ProcessPids([]int{4242}, nil, false, "", nil)
	assert.Empty(t, rUrls)
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "PID 4242 is a Java process (java.exe), but -appRuntime nodejs was passed")
	}
}
