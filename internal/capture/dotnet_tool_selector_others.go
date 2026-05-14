//go:build !windows

package capture

func detectTargetArch(pid int) (string, error) {
	// pid is unused on non-Windows;
	// kept to match the Windows signature, discarded here to silence unusedparams lint
	_ = pid
	return "", nil
}
