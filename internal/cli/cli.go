package cli

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

	runJattachCaptureModesIfConditionSatisfied()

	logCLIArguments()

	if config.GlobalConfig.ShowVersion {
		logger.Log("yc-360 script version: %s", executils.SCRIPT_VERSION)
		return
	}

	err := validate()
	if err == ErrInvalidArgumentCantContinue {
		config.ShowUsage()
		os.Exit(1)
	}

	err = runToCompletionOrSigterm(agent.Run)
	if err != nil {
		logger.Err(err).Msg("Error encountered")
	}

	logger.Log("yc-360 script is shutting down...")
	agent.Shutdown()

	if err != nil {
		os.Exit(1)
	}
}

func runToCompletionOrSigterm(f func() error) error {
	// Setup OS signal channel
	osSigChan := make(chan os.Signal, 1)
	signal.Notify(osSigChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	completed := make(chan error, 1)
	var err error

	go func(completed chan error) {
		err := f()
		completed <- err
	}(completed)

	// Wait for either completion or sigterm signals
	select {
	case s := <-osSigChan:
		logger.Log("Received OS signal: %s. Stopping: a capture in flight ends early and "+
			"uploads what it has captured. Signal again to exit without waiting for it.", s)

		go exitOnSignal(osSigChan)
	case err = <-completed:
	}

	return err
}

// exitOnSignal exits at once on the next signal. A signal Notify catches no longer
// ends the process by default, so without this a stop that takes too long - an
// upload that hangs - could be ended only by SIGKILL.
func exitOnSignal(osSigChan <-chan os.Signal) {
	s := <-osSigChan
	logger.Log("Received OS signal: %s again. Exiting at once: the capture's remaining "+
		"uploads are abandoned, and its directory is left in place.", s)
	os.Exit(1)
}

func logCLIArguments() {
	logger.Log("CLI Arguments: %v", os.Args)
}
