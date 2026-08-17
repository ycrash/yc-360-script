package agent

import (
	"errors"
	"net"
	"strconv"
	"sync"
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
var ErrConflictingMode = errors.New("conflicting mode")

var m3AppMu sync.Mutex
var runningM3App *m3.M3App

func Run() error {
	startupLogs()

	onDemandMode := len(config.GlobalConfig.Pid) > 0
	m3Mode := config.GlobalConfig.M3
	apiMode := config.GlobalConfig.Port > 0

	// A configured postgres: block is itself the target switch.
	dbTargetMode := config.GlobalConfig.Postgres.IsConfigured()

	if err := checkRunTargets(onDemandMode, m3Mode, apiMode, dbTargetMode); err != nil {
		return err
	}

	// TODO: This is for backward compatibility: API mode can run along with on demand and M3.
	// Nobody of us knows whether there's any customer using this (on demand + API mode)
	// I think we should clean it up eventually.
	// On demand (short lived) run along with API mode feels strange.
	// To clean it up: API mode can run standalone or along with M3, but not with on demand.
	if apiMode {
		go runAPIMode()
	}

	// Database-only runs share the on-demand path; checkRunTargets already excludes an app target here.
	if onDemandMode || dbTargetMode {
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
	m3AppMu.Lock()
	if runningM3App != nil {
		runningM3App.Shutdown()
		runningM3App = nil
	}
	m3AppMu.Unlock()

	ondemand.Wg.Wait()
	executils.RemoveFromTempPath()
}

func startupLogs() {
	logger.Log("yc-360 script starting...")

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
		logger.Warn().Msg(err.Error())
	}
}

func runM3Mode() {
	logger.Log("Running M3 mode")

	m3App := m3.NewM3App()
	m3AppMu.Lock()
	runningM3App = m3App
	m3AppMu.Unlock()

	defer func() {
		m3AppMu.Lock()
		if runningM3App == m3App {
			runningM3App = nil
		}
		m3AppMu.Unlock()
	}()

	m3App.RunLoop()
}

// checkRunTargets validates which capture targets a run may combine.
func checkRunTargets(onDemandMode, m3Mode, apiMode, dbTargetMode bool) error {
	// Validation: abort if no mode is specified (M3, OnDemand, API, or a database target).
	if !onDemandMode && !apiMode && !m3Mode && !dbTargetMode {
		logger.Warn().Msg("M3 mode is not enabled. API mode is not enabled. No postgres: block is " +
			"configured. The yc-360 script is about to run OnDemand mode but no PID is specified.")

		return ErrNothingCanBeDone
	}

	// Validation: if ondemand and m3 are both enabled, abort here
	// Because it causes an issue with capture dir in ondemand.go
	if onDemandMode && m3Mode {
		logger.Error().Msg("OnDemand and M3 mode can not run together.")

		return ErrConflictingMode
	}

	// Validation: database and app targets are separate runs, refused rather than
	// picking a winner, which would silently produce an incomplete bundle.
	if dbTargetMode && (onDemandMode || m3Mode || apiMode) {
		logger.Error().Msg("A postgres: block and an application target can not run together - " +
			"the database capture is a separate run. Use a configuration file with no postgres: " +
			"block for the application capture, or drop -p/-m3/-port for the database capture.")

		return ErrConflictingMode
	}

	return nil
}

func runOnDemandMode() {
	pidStr := config.GlobalConfig.Pid

	switch {
	case pidStr == "":
		// Only reachable for a configured postgres: block.
		logger.Log("Running database-only capture (no PID; postgres: block configured)")
	case config.GlobalConfig.OnlyCapture:
		// OnlyCapture mode is technically the same code path as on demand,
		// but the artifacts aren't uploaded.
		// This is only for clarity / avoid confusion in the log.
		logger.Log("Running OnlyCapture mode with PID: %s", pidStr)
	default:
		logger.Log("Running OnDemand mode with PID: %s", pidStr)
	}

	for i, pid := range resolveCapturePids(pidStr) {
		// SkipPostgres for i>0: one database capture per run, not per pid.
		var opts []ondemand.CaptureOptions
		if i > 0 {
			opts = append(opts, ondemand.CaptureOptions{SkipPostgres: true})
		}

		ondemand.FullCapture(pid, config.GlobalConfig.AppName, config.GlobalConfig.HeapDump, config.GlobalConfig.Tags, "", opts...)
	}
}

// resolveCapturePids turns the -p value into pids to capture.
// An empty string is handled directly rather than falling through to the
// token match, which would otherwise match every process on the host.
func resolveCapturePids(pidStr string) []int {
	if pidStr == "" {
		// Pid 0 signals no target process; FullCapture skips process-dependent captures.
		return []int{0}
	}

	if pid, err := strconv.Atoi(pidStr); err == nil {
		return []int{pid}
	}

	// Not an integer, so it probably contains a process token, i.e: buggyApp
	return resolvePidsFromToken(pidStr)
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
