package cli

import (
	"errors"
	"log"
	"os"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

func initConfig() {
	err := config.ParseFlags(os.Args)

	if errors.Is(err, config.ErrHelp) {
		config.ShowUsage()
		os.Exit(0)
	}

	// A bad argument (e.g. -p with no value): print one clear line rather
	// than the flag package's terse error followed by the full help text.
	var argErr *config.ArgError
	if errors.As(err, &argErr) {
		logger.Error().Msg(config.WithArgumentsHelpHint(argErr.Msg))
		os.Exit(1)
	}

	// Anything else, e.g. a YAML error in the -c file: same ERR format.
	if err != nil {
		logger.Error().Msg(err.Error())
		os.Exit(1)
	}
}

// logArgWarnings logs command-line problems that don't stop the run, such
// as a stray word after the last argument. Called after initLogger so the
// warnings also reach the log file.
func logArgWarnings() {
	for _, w := range config.ArgWarnings {
		logger.Warn().Msg(w)
	}
}

func initLogger() {
	err := logger.Init(
		config.GlobalConfig.LogFilePath,
		config.GlobalConfig.LogFileMaxCount,
		config.GlobalConfig.LogFileMaxSize,
		config.GlobalConfig.LogLevel,
	)

	if err != nil {
		log.Fatal(err.Error())
	}
}
