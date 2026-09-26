//go:build !windows

package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configFileWithMode(t *testing.T, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "db.yaml")
	require.NoError(t, os.WriteFile(path, []byte("options:\n  postgres:\n    host: db\n"), 0o600))

	// Explicitly, since WriteFile's mode is masked by the umask.
	require.NoError(t, os.Chmod(path, mode))

	return path
}

func TestCheckConfigFilePermissionsAcceptsOwnerOnly(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o400, 0o700, 0o200} {
		assert.NoError(t, CheckConfigFilePermissions(configFileWithMode(t, mode)), "%04o", mode)
	}
}

func TestCheckConfigFilePermissionsRefusesAnyGroupOrOtherAccess(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o620, 0o610, 0o602, 0o601, 0o666, 0o777} {
		path := configFileWithMode(t, mode)

		err := CheckConfigFilePermissions(path)
		require.Error(t, err, "%04o", mode)

		assert.Contains(t, err.Error(), "chmod 600 "+path, "%04o: the message gives the fix", mode)
		assert.Contains(t, err.Error(), path, "%04o: and names the file", mode)
	}
}

func TestCheckConfigFilePermissionsNamesTheMode(t *testing.T) {
	err := CheckConfigFilePermissions(configFileWithMode(t, 0o644))
	require.Error(t, err)

	assert.Contains(t, err.Error(), "(mode 0644)")
	assert.Contains(t, err.Error(), "open to group or others")
}

func TestCheckConfigFilePermissionsFollowsASymlinkToItsFile(t *testing.T) {
	target := configFileWithMode(t, 0o644)
	link := filepath.Join(t.TempDir(), "link.yaml")
	require.NoError(t, os.Symlink(target, link))

	assert.Error(t, CheckConfigFilePermissions(link), "the file read is the target, whatever the link's own mode")

	require.NoError(t, os.Chmod(target, 0o600))
	assert.NoError(t, CheckConfigFilePermissions(link))
}

func TestCheckConfigFilePermissionsWithNoFile(t *testing.T) {
	assert.NoError(t, CheckConfigFilePermissions(""), "no path: the settings did not come from a file")

	err := CheckConfigFilePermissions(filepath.Join(t.TempDir(), "missing.yaml"))
	assert.Error(t, err, "a path that cannot be read is not passed")
}
