package cli

import (
	"log"
	"os"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
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
