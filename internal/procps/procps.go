//go:build linux
// +build linux

package procps

import "shell/internal/procps/linux"

var VMStat = linux.VMStat
var Top = linux.Top
