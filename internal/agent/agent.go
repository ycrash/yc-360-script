package agent

import (
	"errors"
	"net"
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

var ErrNothingCanBeDone = errors.New("nothing can be done")

func Run() error {
	startupLogs()

	onDemandMode := len(config.GlobalConfig.Pid) > 0
	m3Mode := config.GlobalConfig.M3
	apiMode := config.GlobalConfig.Port > 0

	// Validation: if no mode is specified (neither M3, OnDemand, nor API Mode), abort here
	if !onDemandMode && !apiMode && !m3Mode {
		logger.Log("WARNING: M3 mode is not enabled. API mode is not enabled. The yc-360 script is about to run OnDemand mode but no PID is specified.")

		return ErrNothingCanBeDone
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

	return nil
}

func Shutdown() {
	ondemand.Wg.Wait()
	executils.RemoveFromTempPath()
}

func startupLogs() {
	logger.Log("yc-360 script version: " + executils.SCRIPT_VERSION)
	logger.Log("yc=360 script starting...")

	msg, ok := common.StartupAttend()
	logger.Log(
		`startup attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
}

func runAPIMode() {
	apiServer := api.NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
	logger.Log("Running API mode on %s", net.JoinHostPort(config.GlobalConfig.Address, strconv.Itoa(config.GlobalConfig.Port)))

	err := apiServer.Serve()
	if err != nil {
		logger.Log("WARNING: %s", err)
	}
}

func runM3Mode() {
	logger.Log("Running M3 mode")

	m3App := m3.NewM3App()
	m3App.RunLoop()
}

func runOnDemandMode() {
	pidStr := config.GlobalConfig.Pid
	logger.Log("Running OnDemand mode with PID: %s", pidStr)

	pidInt, err := strconv.Atoi(pidStr)
	pids := []int{}

	if err == nil {
		pids = append(pids, pidInt)
	} else {
		// if pidStr is not an integer, it probably contains a process token, i.e: buggyApp
		resolvedPids := resolvePidsFromToken(pidStr)
		pids = append(pids, resolvedPids...)
	}

	for _, pid := range pids {
		ondemand.FullCapture(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags, "")
	}
}

func resolvePidsFromToken(pidToken string) []int {
	pids := []int{}
	resolvedPids, err := capture.GetProcessIds(config.ProcessTokens{config.ProcessToken(pidToken)}, nil)

	if err != nil {
		logger.Log("unexpected error while resolving PIDs %s", err)
		return pids
	}

	if len(resolvedPids) == 0 {
		logger.Log("failed to find the target process by unique token: %s", config.GlobalConfig.Pid)
		return pids
	}

	for resolvedPid := range resolvedPids {
		if resolvedPid < 1 {
			continue
		}

		pids = append(pids, resolvedPid)
	}

	return pids
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
