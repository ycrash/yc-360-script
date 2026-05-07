//go:build !windows

package capture

func resolveDotnetToolByPid(pid int) (string, bool, error) {
	return "", false, nil
}
