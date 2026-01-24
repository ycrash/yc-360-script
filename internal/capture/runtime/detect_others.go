//go:build !windows

package runtime

// DetectRuntime is a stub for non-Windows platforms.
// Runtime detection is only supported on Windows.
// Returns nil, nil to indicate detection is not available (not an error condition).
// The caller should default to Java when receiving nil.
func DetectRuntime(pid int) (*RuntimeInfo, error) {
	return nil, nil
}
