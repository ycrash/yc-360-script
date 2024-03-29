package cli

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import "C"
import (
	"log"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"shell/internal/agent"
	"shell/internal/config"
	"shell/internal/logger"
	"shell/internal/procps"
	ycattach "shell/internal/ycattach"
)

func Run() {
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
	err := config.ParseFlags(os.Args)
	if err != nil {
		log.Fatal(err.Error())
	}
	err = logger.Init(config.GlobalConfig.LogFilePath, config.GlobalConfig.LogFileMaxCount,
		config.GlobalConfig.LogFileMaxSize, config.GlobalConfig.LogLevel)
	if err != nil {
		log.Fatal(err.Error())
	}

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

	validate()

	osSig := make(chan os.Signal, 1)
	signal.Notify(osSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	go agent.Run()

	<-osSig
	logger.Log("Received kill signal, agent is shutting down...")
	agent.Shutdown()
}
