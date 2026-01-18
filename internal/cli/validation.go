package cli

import "C"
import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

var ErrInvalidArgumentCantContinue = errors.New("cli: invalid argument")

func validate() error {
	// Server URL and API Key
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

	// JAVA_HOME
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
		return ErrInvalidArgumentCantContinue
	}

	// Runtime-specific validation
	switch config.GlobalConfig.AppRuntime {
	case "dotnet":
		if runtime.GOOS != "windows" {
			logger.Warn().Str("os", runtime.GOOS).Msg(".NET capture is only supported on Windows")
			return ErrInvalidArgumentCantContinue
		}

		// Tool path resolution and validation
		toolPath := config.GlobalConfig.DotnetToolPath
		if toolPath == "" {
			if runtime.GOOS == "windows" {
				toolPath = "yc-360-tool-dotnet.exe"
			} else {
				toolPath = "yc-360-tool-dotnet"
			}
			config.GlobalConfig.DotnetToolPath = toolPath
		}

		// Check if tool exists (using exec.LookPath for PATH resolution)
		_, err := exec.LookPath(toolPath)
		if err != nil {
			logger.Warn().Str("path", toolPath).Msgf("%s executable not found", toolPath)
			logger.Warn().Msgf("Please ensure %s is in the same directory as yc-360-script or in your system PATH", toolPath)
			logger.Warn().Msg("Alternatively, specify the path using -dotnetToolPath argument")
			return ErrInvalidArgumentCantContinue
		}
	case "java":
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
		}
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
			return ErrInvalidArgumentCantContinue
		}
	}

	// M3
	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Warn().Msg("-onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}

	if len(config.GlobalConfig.ProcessTokens) > 0 && !config.GlobalConfig.M3 {
		logger.Warn().Msg("ProcessTokens is passed without M3 mode. See https://docs.ycrash.io/yc-360/launch-modes/m3-mode.html")
	}

	// AppLog
	if config.GlobalConfig.AppLogLineCount < -1 {
		logger.Log("%d is not a valid value for 'appLogLineCount' argument. It should be -1 (all lines), 0 (no logs), or a positive number.", config.GlobalConfig.AppLogLineCount)
		return ErrInvalidArgumentCantContinue
	}

	// Validate edDataFolder is not the current working directory
	if config.GlobalConfig.EdDataFolder != "" {
		currentDir, err := os.Getwd()
		if err != nil {
			logger.Log("Failed to get current working directory: %v", err)
			return ErrInvalidArgumentCantContinue
		}

		edDataFolderAbs, err := filepath.Abs(config.GlobalConfig.EdDataFolder)
		if err != nil {
			logger.Log("Failed to resolve edDataFolder path '%s': %v", config.GlobalConfig.EdDataFolder, err)
			return ErrInvalidArgumentCantContinue
		}

		currentDirAbs, err := filepath.Abs(currentDir)
		if err != nil {
			logger.Log("Failed to resolve current directory path: %v", err)
			return ErrInvalidArgumentCantContinue
		}

		if edDataFolderAbs == currentDirAbs {
			logger.Log("ERROR: edDataFolder cannot be the current working directory")
			logger.Log("Current directory: %s", currentDirAbs)
			logger.Log("edDataFolder: %s", edDataFolderAbs)
			return ErrInvalidArgumentCantContinue
		}
	}

	// Validate tls cert
	if (config.GlobalConfig.TLSCertPath != "" && config.GlobalConfig.TLSKeyPath == "") ||
		(config.GlobalConfig.TLSCertPath == "" && config.GlobalConfig.TLSKeyPath != "") {
		logger.Error().Msg("both -tlsCertPath and -tlsKeyPath must be specified for mTLS")
		return ErrInvalidArgumentCantContinue
	}

	// Convert TLS certificate paths to absolute paths and verify they exist
	if config.GlobalConfig.CACertPath != "" {
		absPath, err := filepath.Abs(config.GlobalConfig.CACertPath)
		if err != nil {
			logger.Error().Msgf("Failed to resolve caCertPath '%s': %v", config.GlobalConfig.CACertPath, err)
			return ErrInvalidArgumentCantContinue
		}
		if _, err := os.Stat(absPath); err != nil {
			logger.Error().Msgf("CA certificate file not found at '%s'", absPath)
			return ErrInvalidArgumentCantContinue
		}
		config.GlobalConfig.CACertPath = absPath
	}

	if config.GlobalConfig.TLSCertPath != "" {
		absPath, err := filepath.Abs(config.GlobalConfig.TLSCertPath)
		if err != nil {
			logger.Error().Msgf("Failed to resolve tlsCertPath '%s': %v", config.GlobalConfig.TLSCertPath, err)
			return ErrInvalidArgumentCantContinue
		}
		if _, err := os.Stat(absPath); err != nil {
			logger.Error().Msgf("TLS certificate file not found at '%s'", absPath)
			return ErrInvalidArgumentCantContinue
		}
		config.GlobalConfig.TLSCertPath = absPath
	}

	if config.GlobalConfig.TLSKeyPath != "" {
		absPath, err := filepath.Abs(config.GlobalConfig.TLSKeyPath)
		if err != nil {
			logger.Error().Msgf("Failed to resolve tlsKeyPath '%s': %v", config.GlobalConfig.TLSKeyPath, err)
			return ErrInvalidArgumentCantContinue
		}
		if _, err := os.Stat(absPath); err != nil {
			logger.Error().Msgf("TLS key file not found at '%s'", absPath)
			return ErrInvalidArgumentCantContinue
		}
		config.GlobalConfig.TLSKeyPath = absPath
	}

	return nil
}
