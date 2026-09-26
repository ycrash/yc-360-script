//go:build windows

package config

// CheckConfigFilePermissions accepts every file on Windows: access there is an
// ACL, not the owner, group and other bits the check reads, which Go reports
// only as read-only or not.
func CheckConfigFilePermissions(string) error {
	return nil
}
