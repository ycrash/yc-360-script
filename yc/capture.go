package main

import (
	"bytes"
	"fmt"

	"shell"
	"shell/config"
	"shell/logger"
)

func runCaptureCmd(pid int, cmd string) (output []byte, err error) {
	shell.Env = []string{fmt.Sprintf("pid=%d", pid)}
	output, err = shell.CommandCombinedOutput(shell.SHELL, cmd)
	logger.Log(`run capture cmd: %s
pid: %d
result: %s
err: %v
`, cmd, pid, output, err)
	return
}

func runGCCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.GCCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = runCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func runTDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.TDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = runCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func runHDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.HDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = runCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func updatePaths(pid int) {
	var path []byte
	path, _ = runGCCaptureCmd(pid)
	if len(path) > 0 {
		config.GlobalConfig.GCPath = string(path)
	}
	path, _ = runTDCaptureCmd(pid)
	if len(path) > 0 {
		config.GlobalConfig.ThreadDumpPath = string(path)
	}
	path, _ = runHDCaptureCmd(pid)
	if len(path) > 0 {
		config.GlobalConfig.HeapDumpPath = string(path)
	}
}
