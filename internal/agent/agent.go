package agent

import (
	"os"
	"strconv"
	"yc-agent/internal/agent/api"
	"yc-agent/internal/agent/common"
	"yc-agent/internal/agent/m3"
	"yc-agent/internal/agent/ondemand"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

func Run() {
	startupLogs()

	onDemandMode := len(config.GlobalConfig.Pid) > 0
	m3Mode := config.GlobalConfig.M3
	apiMode := config.GlobalConfig.Port > 0

	// Validation: if no mode is specified (neither M3, OnDemand, nor API Mode), abort here
	if !onDemandMode && !apiMode && !m3Mode {
		// TODO: improve log message to describe why nothing can be done, what
		// should the user do instead?
		logger.Log("WARNING: nothing can be done")

		// TODO: better to return error than exit here
		os.Exit(1)
	}

	// TODO: This is for backward compatibility: API mode can run along with on demand and M3.
	// Nobody of us knows whether there's any customer using this (on demand + API mode)
	// I think we should clean it up eventually.
	// On demand (short lived) run along with API mode feels strange.
	// To clean it up: API mode can run standalone or along with M3, but not with on demand.
	if apiMode {
		go runAPIMode()
	}

	if onDemandMode {
		runOnDemandMode()
	} else {
		if m3Mode {
			go runM3Mode()
		}

		if m3Mode || apiMode {
			// M3 and API mode keep running until the process is killed with a SIGTERM signal,
			// so they need to block here
			for {
				dailyAttendance()
			}
		}
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

func runAPIMode() {
	apiServer, err := api.NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
	if err != nil {
		logger.Log("WARNING: %s", err)
		return
	}

	apiServer.ProcessPids = ondemand.ProcessPids

	err = apiServer.Serve()
	if err != nil {
		logger.Log("WARNING: %s", err)
	}
}

func runM3Mode() {
	m3App := m3.NewM3App()
	m3App.RunLoop()
}

func runOnDemandMode() {
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
}

func dailyAttendance() {
	msg, ok := common.Attend()
	logger.Log(
		`daily attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
}
