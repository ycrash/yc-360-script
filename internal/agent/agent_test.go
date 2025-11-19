package agent

import (
	"testing"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
)

func TestRun(t *testing.T) {
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
}

func TestModeValidation(t *testing.T) {
	// Save original config and restore after tests
	originalConfig := config.GlobalConfig
	defer func() {
		config.GlobalConfig = originalConfig
	}()

	// Initialize logger for tests
	logger.Init("", 0, 0, "info")

	// Create temp directory for test artifacts
	tempDir := t.TempDir()

	tests := []struct {
		name        string
		pid         string
		m3          bool
		port        int
		expectedErr error
		description string
	}{
		{
			name:        "no mode specified",
			pid:         "",
			m3:          false,
			port:        0,
			expectedErr: ErrNothingCanBeDone,
			description: "neither ondemand, m3, nor api mode is enabled",
		},
		{
			name:        "ondemand only",
			pid:         "12345",
			m3:          false,
			port:        0,
			expectedErr: nil,
			description: "valid ondemand mode",
		},
		{
			name:        "m3 only",
			pid:         "",
			m3:          true,
			port:        0,
			expectedErr: nil,
			description: "valid m3 mode",
		},
		{
			name:        "api only",
			pid:         "",
			m3:          false,
			port:        8080,
			expectedErr: nil,
			description: "valid api mode",
		},
		{
			name:        "ondemand and m3 conflict",
			pid:         "12345",
			m3:          true,
			port:        0,
			expectedErr: ErrConflictingMode,
			description: "ondemand and m3 cannot run together",
		},
		{
			name:        "m3 and api together",
			pid:         "",
			m3:          true,
			port:        8080,
			expectedErr: nil,
			description: "m3 and api can run together",
		},
		{
			name:        "ondemand and api together",
			pid:         "12345",
			m3:          false,
			port:        8080,
			expectedErr: nil,
			description: "ondemand and api can run together (backward compatibility)",
		},
		{
			name:        "all three modes",
			pid:         "12345",
			m3:          true,
			port:        8080,
			expectedErr: ErrConflictingMode,
			description: "ondemand conflicts with m3 even with api mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.GlobalConfig = config.Config{
				Options: config.Options{
					Pid:          tt.pid,
					M3:           tt.m3,
					Port:         tt.port,
					Address:      "localhost",
					OnlyCapture:  true, // Required to avoid upload logic in ondemand mode
					JavaHomePath: "/usr/lib/jvm/java-11",
					StoragePath:  tempDir, // Use temp directory for test artifacts
				},
			}

			// Create a channel to catch the error or timeout
			errChan := make(chan error, 1)

			go func() {
				err := Run()
				errChan <- err
			}()

			// Wait for error or timeout
			select {
			case err := <-errChan:
				assert.Equal(t, tt.expectedErr, err, tt.description)
			case <-time.After(100 * time.Millisecond):
				// If we timeout, it means Run() is blocking (which is expected for m3/api modes)
				assert.Nil(t, tt.expectedErr, "%s: expected error %v, but Run() is blocking", tt.description, tt.expectedErr)
				// This is expected behavior for valid m3/api modes - they block indefinitely
			}
		})
	}
}

func TestResolvePidsFromToken(t *testing.T) {
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

func TestModeLogic(t *testing.T) {
	// Save original config and restore after tests
	originalConfig := config.GlobalConfig
	defer func() {
		config.GlobalConfig = originalConfig
	}()

	// Initialize logger for tests
	logger.Init("", 0, 0, "info")
}
