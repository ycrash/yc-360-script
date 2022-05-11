//go:build linux
// +build linux

package procps

import "shell/procps/linux"

var VMStat = linux.VMStat
var Top = linux.Top
