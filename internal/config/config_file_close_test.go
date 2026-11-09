package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// ParseFlags must close the config file once it is read. On Windows an open
// handle would stop the file from being deleted or replaced while a long
// running M3 or API mode yc is still going.
func TestParseFlagsClosesConfigFile(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })

	for _, content := range []string{
		"version: '1'\noptions:\n  s: http://localhost:8080\n",
		"version: '1'\noptions:\n  s: [not, a, string]\n", // decode error path
	} {
		path := filepath.Join(t.TempDir(), "yc-config.yaml")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

		GlobalConfig = defaultConfig()
		_ = ParseFlags([]string{"yc", "-c", path})

		require.NoError(t, os.Remove(path), "the config file is still open after ParseFlags")
	}
}
