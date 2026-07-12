//go:build !linux && !darwin

package capture

import "fmt"

// resolveNodeStdoutFile has no implementation on this platform (notably
// Windows).
func resolveNodeStdoutFile(pid int) (string, error) {
	return "", fmt.Errorf("resolving process stdout (fd 1) is not supported on this platform")
}
