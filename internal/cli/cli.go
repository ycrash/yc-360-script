package cli

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import "C"
import (
	"os"
	"os/signal"
	"syscall"

	"yc-agent/internal/agent"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// Run runs the CLI program. It's responsible to validate the process args pre-condition,
// init core components, run the program in non-primary mode such as top, vmstat, and other capture modes.
// Finally, it runs the primary feature: yc-agent and wait for completion or abort on a sigterm signal.
func Run() {
	if len(os.Args) < 2 {
		logger.Log("No arguments are passed.")
		config.ShowUsage()
		os.Exit(1)
	}

	runRawCaptureModeIfConditionSatisfied()

	initConfig()
	initLogger()

	runCaptureModeIfConditionSatisfied()

	if config.GlobalConfig.ShowVersion {
		logger.Log("yc agent version: " + executils.SCRIPT_VERSION)
		os.Exit(0)
	}

	validate()

	// The main feature: run agent and wait for either completion or a sigterm signal
	osSig := make(chan os.Signal, 1)
	signal.Notify(osSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	go agent.Run()

	<-osSig
	logger.Log("Received kill signal, agent is shutting down...")
	agent.Shutdown()
}
