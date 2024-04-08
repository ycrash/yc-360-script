package cli

import (
	"log"
	"os"
	"shell/internal/config"
	"shell/internal/logger"
)

func initConfig() {
	err := config.ParseFlags(os.Args)

	if err != nil {
		log.Fatal(err.Error())
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
