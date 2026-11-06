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
		logger.Error().Msg("No arguments were passed. See the options below.")
		config.ShowUsage()
		os.Exit(1)
	}

	runRawCaptureModeIfConditionSatisfied()

	initConfig()
	initLogger()

	runJattachCaptureModesIfConditionSatisfied()

	logCLIArguments()
	logArgWarnings()

	if config.GlobalConfig.ShowVersion {
		logger.Log("yc-360 script version: %s", executils.SCRIPT_VERSION)
		return
	}

	err := validate()
	if err == ErrInvalidArgumentCantContinue {
		config.ShowUsage()
		// The usage text is long; repeat why yc stopped so it is the last line shown.
		if invalidArgReason != "" {
			logger.Error().Msg(invalidArgReason)
		}
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

	completed := make(chan error)
	var err error

	go func(completed chan error) {
		err := f()
		completed <- err
	}(completed)

	// Wait for either completion or sigterm signals
	select {
	case s := <-osSigChan:
		logger.Log("Received OS signal: %s", s)
	case err = <-completed:
	}

	return err
}

func logCLIArguments() {
	// Redact secrets such as the -k API key: this line goes to the console,
	// the log file, and (in on-demand mode) the uploaded yc360Logs.
	logger.Log("CLI Arguments: %v", config.RedactArgs(os.Args))
}
