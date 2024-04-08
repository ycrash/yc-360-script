//go:build darwin || linux
// +build darwin linux

package ycattach

import "yc-agent/internal/capture/ycattach/posix"

var Capture = posix.Capture
var CaptureThreadDump = posix.CaptureThreadDump
var CaptureHeapDump = posix.CaptureHeapDump
var CaptureGCLog = posix.CaptureGCLog
