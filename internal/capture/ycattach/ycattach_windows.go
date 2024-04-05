//go:build windows
// +build windows

package ycattach

import "shell/internal/capture/ycattach/windows"

var Capture = windows.Capture
var CaptureThreadDump = windows.CaptureThreadDump
var CaptureHeapDump = windows.CaptureHeapDump
var CaptureGCLog = windows.CaptureGCLog
