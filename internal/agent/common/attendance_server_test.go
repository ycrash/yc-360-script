package common

import (
	"testing"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAttendanceIsWhatTheServerExpects(t *testing.T) {
	for _, m3 := range []bool{false, true} {
		srv := fakeserver.Start(t, fakeserver.Options{})

		previous := config.GlobalConfig
		t.Cleanup(func() { config.GlobalConfig = previous })

		config.GlobalConfig.OnlyCapture = false
		config.GlobalConfig.M3 = m3
		config.GlobalConfig.Server = srv.URL()
		config.GlobalConfig.ApiKey = srv.APIKey()
		config.GlobalConfig.HttpClientTimeout = config.Duration(10 * time.Second)

		msg, ok := StartupAttend()
		require.True(t, ok, msg)

		requests := srv.Requests()
		require.Len(t, requests, 1)
		assert.Equal(t, "startup", requests[0].Query.Get("type"))

		if m3 {
			assert.Equal(t, "true", requests[0].Query.Get("m3"), "an M3 run says so")
		} else {
			assert.NotContains(t, requests[0].Query, "m3")
		}

		assert.Empty(t, srv.Violations(), "m3=%t", m3)
	}
}
