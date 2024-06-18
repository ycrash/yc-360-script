package cli

import "C"
import (
	"errors"
	"os"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

var ErrInvalidArgumentCantContinue = errors.New("cli: invalid argument")

func validate() error {
	if !config.GlobalConfig.OnlyCapture {
		if len(config.GlobalConfig.Server) < 1 {
			logger.Log("'-s' yCrash server URL argument not passed.")
			return ErrInvalidArgumentCantContinue
		}
		if len(config.GlobalConfig.ApiKey) < 1 {
			logger.Log("'-k' yCrash API Key argument not passed.")
			return ErrInvalidArgumentCantContinue
		}
	}

	if len(config.GlobalConfig.JavaHomePath) < 1 {
		config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
		return ErrInvalidArgumentCantContinue
	}

	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Log("WARNING: -onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}

	if config.GlobalConfig.AppLogLineCount < 1 {
		logger.Log("%d is not a valid value for 'appLogLineCount' argument. It should be a number larger than 0.", config.GlobalConfig.AppLogLineCount)
		return ErrInvalidArgumentCantContinue
	}

	return nil
}
