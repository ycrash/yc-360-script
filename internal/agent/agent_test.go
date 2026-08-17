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

	t.Run("a configured postgres block conflicts with an application target", func(t *testing.T) {
		for _, tt := range []struct {
			name    string
			options config.Options
		}{
			{"with a PID", config.Options{Pid: "12345", Postgres: &config.Postgres{Host: "db"}}},
			{"with a process token", config.Options{Pid: "buggyApp", Postgres: &config.Postgres{Host: "db"}}},
			{"with M3", config.Options{M3: true, Postgres: &config.Postgres{Host: "db"}}},
			{"with API mode", config.Options{Port: 8080, Postgres: &config.Postgres{Host: "db"}}},
		} {
			t.Run(tt.name, func(t *testing.T) {
				config.GlobalConfig = config.Config{Options: tt.options}

				assert.Equal(t, ErrConflictingMode, Run())
			})
		}
	})
}

func TestCheckRunTargets(t *testing.T) {
	logger.Init("", 0, 0, "info")

	tests := []struct {
		name                                    string
		onDemandMode, m3Mode, apiMode, dbTarget bool
		want                                    error
	}{
		{name: "no target at all", want: ErrNothingCanBeDone},
		{name: "a configured postgres block is a target on its own", dbTarget: true},
		{name: "a PID on its own", onDemandMode: true},
		{name: "M3 on its own", m3Mode: true},
		{name: "API mode on its own", apiMode: true},
		{name: "ondemand and m3", onDemandMode: true, m3Mode: true, want: ErrConflictingMode},

		{name: "postgres and a PID", onDemandMode: true, dbTarget: true, want: ErrConflictingMode},
		{name: "postgres and M3", m3Mode: true, dbTarget: true, want: ErrConflictingMode},
		{name: "postgres and API mode", apiMode: true, dbTarget: true, want: ErrConflictingMode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, checkRunTargets(tt.onDemandMode, tt.m3Mode, tt.apiMode, tt.dbTarget))
		})
	}
}

func TestResolveCapturePids(t *testing.T) {
	logger.Init("", 0, 0, "info")

	originalConfig := config.GlobalConfig
	defer func() { config.GlobalConfig = originalConfig }()
	config.GlobalConfig = config.Config{}

	t.Run("no PID resolves to the no-process capture", func(t *testing.T) {
		assert.Equal(t, []int{0}, resolveCapturePids(""))
	})

	t.Run("a numeric PID is used as given", func(t *testing.T) {
		assert.Equal(t, []int{123}, resolveCapturePids("123"))
	})

	t.Run("a token that matches nothing captures nothing", func(t *testing.T) {
		assert.Empty(t, resolveCapturePids("nonexistent-token"))
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
