//go:build !windows

package config

import (
	"fmt"
	"os"
)

// CheckConfigFilePermissions refuses a config file that grants any access to
// group or others. It carries the database's host, port, user name and TLS
// file paths in plain text, even when the password is a ${VAR} reference.
// No path means the settings did not come from a file, and there is nothing
// to check.
func CheckConfigFilePermissions(path string) error {
	if path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config file %s: %w", path, err)
	}

	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("config file %s is open to group or others (mode %04o). It holds the "+
			"database connection settings, so only its owner may read it: chmod 600 %s",
			path, mode, path)
	}

	return nil
}
