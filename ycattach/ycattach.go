//go:build darwin || linux
// +build darwin linux

package ycattach

import "shell/ycattach/posix"

var Capture = posix.Capture
var CaptureThreadDump = posix.CaptureThreadDump
var CaptureHeapDump = posix.CaptureHeapDump
var CaptureGCLog = posix.CaptureGCLog
