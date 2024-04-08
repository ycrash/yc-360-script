//go:build linux
// +build linux

package procps

import "yc-agent/internal/capture/procps/linux"

var VMStat = linux.VMStat
var Top = linux.Top
