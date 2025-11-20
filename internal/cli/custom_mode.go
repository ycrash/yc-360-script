package cli

import (
	"os"
	"strconv"
	"yc-agent/internal/capture/procps"
	"yc-agent/internal/capture/ycattach"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// runRawCaptureModeIfConditionSatisfied runs custom capture mode depending on the args.
// Raw capture mode is capture mode that accepts arbitrary arguments, so it should be run
// before parsing config flags.
// For example: -topMode accepts -bc or -bH that will be passed through to the underlying top program.
func runRawCaptureModeIfConditionSatisfied() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-vmstatMode":
			ret := procps.VMStat(os.Args[1:]...)
			os.Exit(ret)
		case "-topMode":
			ret := procps.Top(append([]string{"top"}, os.Args[2:]...)...)
			os.Exit(ret)
		}
	}
}

// runJattachCaptureModesIfConditionSatisfied runs jattach-based capture modes depending on the config.
// These modes (GC, thread dump, heap dump, jcmd) use jattach for direct Java diagnostics.
// This function can be run after parsing the config flags and exits immediately upon execution.
func runJattachCaptureModesIfConditionSatisfied() {
	if config.GlobalConfig.GCCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureGCLog(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.TDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureThreadDump(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.HDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		if len(config.GlobalConfig.HeapDumpPath) <= 0 {
			logger.Log("-hdPath can not be empty")
			os.Exit(1)
		}
		ret := ycattach.CaptureHeapDump(pid, config.GlobalConfig.HeapDumpPath)
		os.Exit(ret)
	}
	if len(config.GlobalConfig.JCmdCaptureMode) > 0 {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.Capture(pid, "jcmd", config.GlobalConfig.JCmdCaptureMode)
		os.Exit(ret)
	}
}
