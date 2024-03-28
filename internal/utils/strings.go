package utils

import "strings"

// CutSuffix returns s without the provided ending suffix string
// and reports whether it found the suffix.
// If s doesn't end with suffix, CutSuffix returns s, false.
// If suffix is the empty string, CutSuffix returns s, true.
// This is a shim for strings.CutPrefix. Once we upgraded go version to a recent one,
// this should be replaced with strings.CutPrefix.
func CutSuffix(s, suffix string) (before string, found bool) {
	if !strings.HasSuffix(s, suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}
