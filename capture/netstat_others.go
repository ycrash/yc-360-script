//go:build !windows && !linux
// +build !windows,!linux

package capture

import (
	"errors"
	"io"
)

func netStat(udp, tcp, ipv4, ipv6, resolve, all, listening bool, writer io.Writer) (err error) {
	return errors.New("netstat is not supported on this platform")
}
