package cli

import (
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidate(t *testing.T) {
	// Save original config and restore after tests
	originalConfig := config.GlobalConfig
	defer func() {
		config.GlobalConfig = originalConfig
	}()

	// Initialize logger for tests (required since validate() calls logger)
	logger.Init("", 0, 0, "info")

	t.Run("missing server URL when not onlyCapture", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  false,
				Server:       "",
				ApiKey:       "test-api-key",
				JavaHomePath: "/usr/lib/jvm/java-11",
			},
		}

		err := validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)
	})

	t.Run("missing API key when not onlyCapture", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  false,
				Server:       "https://ycrash.example.com",
				ApiKey:       "",
				JavaHomePath: "/usr/lib/jvm/java-11",
			},
		}

		err := validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)
	})

	t.Run("onlyCapture mode ignores server and API key", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  true,
				Server:       "",
				ApiKey:       "",
				JavaHomePath: "/usr/lib/jvm/java-11",
			},
		}

		err := validate()
		assert.NoError(t, err)
	})

	t.Run("missing JAVA_HOME", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  true,
				JavaHomePath: "",
			},
		}
		// Ensure JAVA_HOME env var is not set
		os.Unsetenv("JAVA_HOME")

		err := validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)
	})

	t.Run("JAVA_HOME from environment variable", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  true,
				JavaHomePath: "",
				Server:       "https://ycrash.example.com",
				ApiKey:       "test-key",
			},
		}
		// Set JAVA_HOME env var
		os.Setenv("JAVA_HOME", "/usr/lib/jvm/java-11")
		defer os.Unsetenv("JAVA_HOME")

		err := validate()
		assert.NoError(t, err)
		assert.Equal(t, "/usr/lib/jvm/java-11", config.GlobalConfig.JavaHomePath)
	})

	t.Run("m3 mode disables onlyCapture", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				M3:           true,
				OnlyCapture:  true,
				Server:       "https://ycrash.example.com",
				ApiKey:       "test-api-key",
				JavaHomePath: "/usr/lib/jvm/java-11",
			},
		}

		err := validate()
		assert.NoError(t, err)
		assert.False(t, config.GlobalConfig.OnlyCapture, "expected onlyCapture to be disabled in m3 mode")
	})

	t.Run("warning for processTokens without m3 mode", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				M3:            false,
				ProcessTokens: config.ProcessTokens{"buggyApp", "anotherApp"},
				OnlyCapture:   true,
				JavaHomePath:  "/usr/lib/jvm/java-11",
			},
		}

		// This should log a warning but not return an error
		err := validate()
		assert.NoError(t, err)
	})

	t.Run("invalid appLogLineCount negative value", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:     true,
				JavaHomePath:    "/usr/lib/jvm/java-11",
				AppLogLineCount: -5, // Invalid: less than -1
			},
		}

		err := validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)
	})

	t.Run("valid appLogLineCount values", func(t *testing.T) {
		validCounts := []int{-1, 0, 100, 1000}

		for _, count := range validCounts {
			t.Run(filepath.Join("appLogLineCount", string(rune(count))), func(t *testing.T) {
				config.GlobalConfig = config.Config{
					Options: config.Options{
						OnlyCapture:     true,
						JavaHomePath:    "/usr/lib/jvm/java-11",
						AppLogLineCount: count,
					},
				}

				err := validate()
				assert.NoError(t, err, "expected no error for appLogLineCount=%d", count)
			})
		}
	})

	t.Run("edDataFolder cannot be current directory", func(t *testing.T) {
		currentDir, err := os.Getwd()
		require.NoError(t, err, "failed to get current directory")

		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  true,
				JavaHomePath: "/usr/lib/jvm/java-11",
				EdDataFolder: currentDir,
			},
		}

		err = validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)
	})

	t.Run("edDataFolder with relative path resolution", func(t *testing.T) {
		currentDir, err := os.Getwd()
		require.NoError(t, err, "failed to get current directory")

		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:  true,
				JavaHomePath: "/usr/lib/jvm/java-11",
				EdDataFolder: ".", // Current directory via relative path
			},
		}

		err = validate()
		assert.Equal(t, ErrInvalidArgumentCantContinue, err)

		// Test with valid relative path
		config.GlobalConfig.EdDataFolder = filepath.Join(currentDir, "..", "test-folder")
		err = validate()
		assert.NoError(t, err)
	})

	t.Run("valid configuration", func(t *testing.T) {
		config.GlobalConfig = config.Config{
			Options: config.Options{
				OnlyCapture:     false,
				Server:          "https://ycrash.example.com",
				ApiKey:          "test-api-key@12345",
				JavaHomePath:    "/usr/lib/jvm/java-11",
				AppLogLineCount: 100,
			},
		}

		err := validate()
		assert.NoError(t, err)
	})
}
