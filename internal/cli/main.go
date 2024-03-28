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
	"sync"
	"syscall"

	"shell/internal/cli/api"
	"shell/internal/cli/m3"
	"shell/internal/cli/ondemand"
	"shell/internal/config"
	"shell/internal/logger"
	"shell/internal/procps"
	"shell/internal/utils"
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

	go mainLoop()
	defer utils.RemoveFromTempPath()

	select {
	case <-osSig:
		logger.Log("Waiting...")
		ondemand.Wg.Wait()
	}
}

func validate() {
	if len(os.Args) < 2 {
		logger.Log("No arguments are passed.")
		config.ShowUsage()
		os.Exit(1)
	}

	if config.GlobalConfig.ShowVersion {
		logger.Log("yc agent version: " + utils.SCRIPT_VERSION)
		os.Exit(0)
	}

	if !config.GlobalConfig.OnlyCapture {
		if len(config.GlobalConfig.Server) < 1 {
			logger.Log("'-s' yCrash server URL argument not passed.")
			config.ShowUsage()
			os.Exit(1)
		}
		if len(config.GlobalConfig.ApiKey) < 1 {
			logger.Log("'-k' yCrash API Key argument not passed.")
			config.ShowUsage()
			os.Exit(1)
		}
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
		config.ShowUsage()
		os.Exit(1)
	}
	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Log("WARNING: -onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}
	if config.GlobalConfig.AppLogLineCount < 1 {
		logger.Log("%d is not a valid value for 'appLogLineCount' argument. It should be a number larger than 0.", config.GlobalConfig.AppLogLineCount)
		config.ShowUsage()
		os.Exit(1)
	}
}

func mainLoop() {
	var once sync.Once
	if config.GlobalConfig.Port > 0 {
		once.Do(startupLogs)
		go func() {
			s, err := api.NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
			if err != nil {
				logger.Log("WARNING: %s", err)
				return
			}
			s.ProcessPids = ondemand.ProcessPids
			err = s.Serve()
			if err != nil {
				logger.Log("WARNING: %s", err)
			}
		}()
	}

	if config.GlobalConfig.M3 {
		once.Do(startupLogs)
		m3App := m3.NewM3App()
		go func() {
			m3App.RunLoop()
		}()
	} else if len(config.GlobalConfig.Pid) > 0 {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			ids, err := utils.GetProcessIds(config.ProcessTokens{config.ProcessToken(config.GlobalConfig.Pid)}, nil)
			if err == nil {
				if len(ids) > 0 {
					for pid := range ids {
						if pid < 1 {
							continue
						}
						ondemand.FullProcess(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags, "")
					}
				} else {
					logger.Log("failed to find the target process by unique token %s", config.GlobalConfig.Pid)
				}
			} else {
				logger.Log("unexpected error %s", err)
			}
		} else {
			ondemand.FullProcess(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags, "")
		}
		utils.RemoveFromTempPath()
		os.Exit(0)
	} else if config.GlobalConfig.Port <= 0 && !config.GlobalConfig.M3 {
		once.Do(startupLogs)
		logger.Log("WARNING: nothing can be done")
		os.Exit(1)
	}
	for {
		msg, ok := utils.Attend()
		logger.Log(
			`daily attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}
}

func startupLogs() {
	logger.Log("yc agent version: " + utils.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	msg, ok := utils.StartupAttend()
	logger.Log(
		`startup attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
}
