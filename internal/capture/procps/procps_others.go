//go:build !linux

package procps

import "fmt"

func notImplemented(_ ...string) (ret int) {
	_, _ = fmt.Println("Not implemented on this platform")
	return 1
}

var VMStat = notImplemented
var Top = notImplemented
