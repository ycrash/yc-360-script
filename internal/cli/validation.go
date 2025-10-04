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

	// .NET runtime validation
	if config.GlobalConfig.AppRuntime == "dotnet" {
		// Platform check - .NET capture only supported on Windows
		if runtime.GOOS != "windows" {
			logger.Log(".NET capture is only supported on Windows. Current OS: %s", runtime.GOOS)
			logger.Log("Please use -appRuntime=java for non-Windows platforms")
			return ErrInvalidArgumentCantContinue
		}

		// Tool path resolution and validation
		toolPath := config.GlobalConfig.DotnetToolPath
		if toolPath == "" {
			// Auto-discover: use relative path relying on OS PATH resolution
			if runtime.GOOS == "windows" {
				toolPath = "yc-360-tool-dotnet.exe"
			} else {
				toolPath = "yc-360-tool-dotnet"
			}
		}

		// Check if tool exists (using exec.LookPath for PATH resolution)
		resolvedPath, err := exec.LookPath(toolPath)
		if err != nil {
			logger.Log("yc-360-tool-dotnet executable not found: %s", toolPath)
			logger.Log("Please ensure yc-360-tool-dotnet.exe is in the same directory as yc.exe or in your system PATH")
			logger.Log("Alternatively, specify the path using -dotnetToolPath argument")
			return ErrInvalidArgumentCantContinue
		}

		// Store the resolved path back to config for use in capture modules
		config.GlobalConfig.DotnetToolPath = resolvedPath
		logger.Log("Using .NET diagnostic tool at: %s", resolvedPath)
	}

	// Java-specific validation - skip if using .NET runtime
	if config.GlobalConfig.AppRuntime != "dotnet" {
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
		}
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			logger.Log("'-j' yCrash JAVA_HOME argument not passed.")
			return ErrInvalidArgumentCantContinue
		}
	}

	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Log("WARNING: -onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}

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

	return nil
}
