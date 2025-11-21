package agent

import (
	"testing"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
)

func TestRun(t *testing.T) {
	// Tests basic mode validation error cases that return immediately
	// Save original config and restore after tests
	originalConfig := config.GlobalConfig
	defer func() {
		config.GlobalConfig = originalConfig
	}()

	// Initialize logger for tests
	logger.Init("", 0, 0, "info")

	t.Run("no mode specified returns ErrNothingCanBeDone", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				Pid:  "", // No PID (ondemand mode disabled)
				M3:   false,
				Port: 0, // API mode disabled
			},
		}

		err := Run()
		assert.Equal(t, ErrNothingCanBeDone, err)
	})

	t.Run("ondemand and m3 mode conflict", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				Pid:  "12345", // OnDemand mode enabled
				M3:   true,    // M3 mode enabled
				Port: 0,
			},
		}

		err := Run()
		assert.Equal(t, ErrConflictingMode, err)
	})

	t.Run("ondemand mode with process token and m3 mode conflict", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				Pid:  "buggyApp", // OnDemand mode with token
				M3:   true,       // M3 mode enabled
				Port: 0,
			},
		}

		err := Run()
		assert.Equal(t, ErrConflictingMode, err)
	})

	t.Run("all three modes conflict", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				Pid:  "12345", // OnDemand mode enabled
				M3:   true,    // M3 mode enabled
				Port: 8080,    // API mode enabled
			},
		}

		err := Run()
		assert.Equal(t, ErrConflictingMode, err)
	})
}

func TestResolvePidsFromToken(t *testing.T) {
	// Tests PID resolution from process tokens - uses nonexistent token to avoid env dependency
	// Save original config and restore after tests
	originalConfig := config.GlobalConfig
	defer func() {
		config.GlobalConfig = originalConfig
	}()

	// Initialize logger for tests
	logger.Init("", 0, 0, "info")

	t.Run("resolve pids from token with no matches", func(t *testing.T) {
		pids := resolvePidsFromToken("nonexistent-token")

		assert.Empty(t, pids, "expected empty pids slice for nonexistent token")
	})
}
