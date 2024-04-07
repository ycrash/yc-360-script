package agent

import (
	"os"
	"shell/internal/agent/api"
	"shell/internal/agent/common"
	"shell/internal/agent/m3"
	"shell/internal/agent/ondemand"
	"shell/internal/capture"
	"shell/internal/capture/executils"
	"shell/internal/config"
	"shell/internal/logger"
	"strconv"
	"sync"
)

func Run() {
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
			ids, err := capture.GetProcessIds(config.ProcessTokens{config.ProcessToken(config.GlobalConfig.Pid)}, nil)
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
		executils.RemoveFromTempPath()
		os.Exit(0)
	} else if config.GlobalConfig.Port <= 0 && !config.GlobalConfig.M3 {
		once.Do(startupLogs)
		logger.Log("WARNING: nothing can be done")
		os.Exit(1)
	}
	for {
		msg, ok := common.Attend()
		logger.Log(
			`daily attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}
}

func Shutdown() {
	ondemand.Wg.Wait()
	executils.RemoveFromTempPath()
}

func startupLogs() {
	logger.Log("yc agent version: " + executils.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	msg, ok := common.StartupAttend()
	logger.Log(
		`startup attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
}
