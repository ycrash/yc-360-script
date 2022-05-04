//go:build !linux
// +build !linux

package vmstat

import "fmt"

func VMStat(_ ...string) (ret int) {
	_, _ = fmt.Println("Not implemented on this platform")
	return 1
}
