package ondemand

import (
	"testing"
	"yc-agent/internal/capture"
	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewHeapDumpTask(t *testing.T) {
	tests := []struct {
		name         string
		appRuntime   string
		hdPath       string
		hd           bool
		minimalTouch bool
		want         capture.Task
	}{
		{name: "dotnet with -hd", appRuntime: "dotnet", hd: true, want: &capture.DotnetHeapDump{}},
		{name: "dotnet without -hd", appRuntime: "dotnet"},
		{name: "dotnet ignores -hdPath", appRuntime: "dotnet", hdPath: "/tmp/pre-existing.hprof"},
		{name: "dotnet -hd overridden by MinimalTouch", appRuntime: "dotnet", hd: true, minimalTouch: true},
		{name: "nodejs never captures", appRuntime: "nodejs", hd: true},
		{name: "java with -hd", appRuntime: "java", hd: true, want: &capture.HeapDump{}},
		{name: "java without -hd still uploads -hdPath", appRuntime: "java", hdPath: "/tmp/pre-existing.hprof", want: &capture.HeapDump{}},
		{name: "empty runtime falls back to java", hd: true, want: &capture.HeapDump{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prev := config.GlobalConfig.MinimalTouch
			config.GlobalConfig.MinimalTouch = tt.minimalTouch
			t.Cleanup(func() { config.GlobalConfig.MinimalTouch = prev })

			got := newHeapDumpTask(tt.appRuntime, 4321, tt.hdPath, tt.hd)

			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.IsType(t, tt.want, got)

			if d, ok := got.(*capture.DotnetHeapDump); ok {
				assert.Equal(t, 4321, d.Pid)
			}
		})
	}
}
