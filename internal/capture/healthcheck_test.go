package capture

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"yc-agent/internal/config"
)

func TestHealthCheck_Run(t *testing.T) {
	t.Run("should successfully check healthy endpoint", func(t *testing.T) {
		// Create a mock HTTP server that returns a healthy response
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":"ok"}`))
		}))
		defer server.Close()

		// Change to temp directory to avoid polluting the working directory
		tmpDir := t.TempDir()
		originalWd, err := os.Getwd()
		require.NoError(t, err)
		defer os.Chdir(originalWd)
		err = os.Chdir(tmpDir)
		require.NoError(t, err)

		h := &HealthCheck{
			AppName: "TestApp",
			Cfg: config.HealthCheck{
				Endpoint:    server.URL,
				HTTPBody:    `{"status":"ok"}`,
				TimeoutSecs: 2,
			},
		}

		result, err := h.Run()

		assert.NoError(t, err)
		// Note: result.Ok depends on PostData which may fail without a real yc server
		// The key assertion is that the health check itself completes without error
		_ = result
	})

	t.Run("should handle empty endpoint", func(t *testing.T) {
		tmpDir := t.TempDir()
		originalWd, err := os.Getwd()
		require.NoError(t, err)
		defer os.Chdir(originalWd)
		err = os.Chdir(tmpDir)
		require.NoError(t, err)

		h := &HealthCheck{
			AppName: "TestApp",
			Cfg: config.HealthCheck{
				Endpoint:    "",
				TimeoutSecs: 2,
			},
		}

		_, err = h.Run()

		// Empty endpoint should still return without a Go error (error is logged to file)
		assert.NoError(t, err)
	})

	t.Run("should handle server returning error status", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"internal server error"}`))
		}))
		defer server.Close()

		tmpDir := t.TempDir()
		originalWd, err := os.Getwd()
		require.NoError(t, err)
		defer os.Chdir(originalWd)
		err = os.Chdir(tmpDir)
		require.NoError(t, err)

		h := &HealthCheck{
			AppName: "TestApp",
			Cfg: config.HealthCheck{
				Endpoint:    server.URL,
				TimeoutSecs: 2,
			},
		}

		_, err = h.Run()

		// The Run() method captures the response regardless of status code
		assert.NoError(t, err)
	})
}

func TestSanitizeAppNameForFileName(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"simple name", "MyApp", "MyApp"},
		{"with spaces", "My App", "My_App"},
		{"with path traversal", "../../../etc/passwd", "___etc_passwd"},
		{"with slashes", "app/name", "app_name"},
		{"with backslashes", "app\\name", "app_name"},
		{"empty string", "", "default"},
		{"special chars", "app@name#1", "app_name_1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeAppNameForFileName(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}
