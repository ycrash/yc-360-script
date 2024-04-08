package cli

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import "C"
import (
	"os"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

func validate() {
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
