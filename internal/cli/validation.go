package cli

import "C"
import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
	"yc-agent/internal/nodejsvalidation"
)

var ErrInvalidArgumentCantContinue = errors.New("cli: invalid argument")

// invalidArgReason is the message of the last fatal argument error. Run
// prints it again after the usage text, which is long enough to scroll the
// first copy off the screen.
var invalidArgReason string

// invalidArg logs a fatal argument error at error level, remembers it for
// Run, and returns ErrInvalidArgumentCantContinue.
func invalidArg(format string, args ...interface{}) error {
	invalidArgReason = fmt.Sprintf(format, args...)
	logger.Error().Msg(invalidArgReason)
	return ErrInvalidArgumentCantContinue
}

// onlyCaptureHint is appended to the messages for a missing -s or -k.
const onlyCaptureHint = "or add -onlyCapture to save the captured data to a local .zst file instead of uploading it."

func validateNodejs(appRuntime string) error {
	if err := nodejsvalidation.Validate(appRuntime); err != nil {
		return invalidArg("%s", err)
	}
	return nil
}

func validateNodejsSignal(flagName, value string) error {
	if err := nodejsvalidation.ValidateSignal(flagName, value); err != nil {
		return invalidArg("%s", err)
	}
	return nil
}

func validate() error {
	// Checked first: if Windows merged the following arguments into -j, then
	// -p, -s, -k etc. are missing and every later check would report the wrong cause.
	// Only the path part is logged: the merged text may include the -k API key.
	if pathPart, swallowed := javaHomeSwallowedArgs(config.GlobalConfig.JavaHomePath); swallowed {
		// The usage text printed after this error shows current values as flag
		// defaults; keep the merged arguments (and any -k key) out of it.
		config.GlobalConfig.JavaHomePath = pathPart
		return invalidArg(`The -j argument -j "%s\" ends in a backslash. On Windows that turns the closing quote into a literal one, so every argument after it was merged into -j and lost. Remove the trailing backslash (for example -j "%s") and run again.`, pathPart, pathPart)
	}

	appRuntime := config.GetConfiguredAppRuntime()
	if !config.IsValidAppRuntime(appRuntime) {
		// Log the raw (un-normalized) value so the user sees exactly what they typed.
		return invalidArg("The -appRuntime value %q is not supported. Use -appRuntime java, -appRuntime dotnet or -appRuntime nodejs, or remove -appRuntime and the yc-360 script will detect the runtime automatically.", config.GlobalConfig.AppRuntime)
	}

	// Node.js capture-mode / signal validation.
	if err := validateNodejs(appRuntime); err != nil {
		return err
	}

	// Server URL and API Key
	if !config.GlobalConfig.OnlyCapture {
		if len(config.GlobalConfig.Server) < 1 {
			return invalidArg("The yCrash server URL (-s) is required. Pass it with -s, for example -s https://ycrash.example.com, " + onlyCaptureHint)
		}
		if len(config.GlobalConfig.ApiKey) < 1 {
			return invalidArg("The yCrash API key (-k) is required. Pass it with -k, for example -k <your-api-key>, " + onlyCaptureHint)
		}
	}

	// JAVA_HOME validation (only relevant for Java targets).
	if appRuntime != "dotnet" && appRuntime != "nodejs" {
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
		}
		// Tools are resolved as <javaHome>/bin/<tool>, so drop a "bin" the
		// user already included (e.g. -j D:\jdk\bin) instead of looking in bin\bin.
		// This runs before the empty check: a value that was only a stray quote
		// cleans up to "" and must then get the "not set" warning.
		if len(config.GlobalConfig.JavaHomePath) > 0 {
			cleaned, changed := normalizeJavaHome(config.GlobalConfig.JavaHomePath)
			// Captures chdir into the capture folder, so a relative path must be resolved now.
			abs, madeAbs := absJavaHome(cleaned)
			if changed || madeAbs {
				logger.Log("Java home path %q adjusted to %q (JDK tools are looked up under <java home>/bin)", config.GlobalConfig.JavaHomePath, abs)
				config.GlobalConfig.JavaHomePath = abs
			}
		}
		if len(config.GlobalConfig.JavaHomePath) < 1 {
			logger.Warn().Msg("JAVA_HOME not set - Java process captures will fail")
		} else if !javaExecutableExists(config.GlobalConfig.JavaHomePath) {
			logger.Warn().Msgf("Java home path %q has no bin/java - check the -j argument or JAVA_HOME. Captures that need JDK tools (jstat, jcmd, jstack) will fall back to jattach or be skipped", config.GlobalConfig.JavaHomePath)
		}
	}

	// .NET tool validation
	if runtime.GOOS == "windows" && appRuntime == "dotnet" {
		if config.GlobalConfig.DotnetToolPath != "" {
			if err := config.ValidateDotnetToolOverride(); err != nil {
				return invalidArg("%v", err)
			}
		} else {
			warnings, err := config.ValidateDotnetToolInstall()
			if err != nil {
				return invalidArg("%v", err)
			}
			for _, w := range warnings {
				logger.Warn().Msg(w)
			}
		}
	}

	if config.GlobalConfig.Postgres.IsConfigured() && config.GlobalConfig.MySQL.IsConfigured() {
		logger.Error().Msgf("%v", config.ErrBothDatabaseTargets)
		return ErrInvalidArgumentCantContinue
	}

	if config.GlobalConfig.DBTargetConfigured() {
		// Only a database block's config file holds credentials.
		if err := config.CheckConfigFilePermissions(config.GlobalConfig.ConfigPath); err != nil {
			logger.Error().Msgf("%v", err)
			return ErrInvalidArgumentCantContinue
		}
	}

	if pg := config.GlobalConfig.Postgres; pg.IsConfigured() {
		warnings, err := pg.Validate()

		for _, w := range warnings {
			logger.Warn().Msg(w)
		}

		if err != nil {
			logger.Error().Msgf("invalid postgres configuration: %v", err)
			return ErrInvalidArgumentCantContinue
		}
	}

	if my := config.GlobalConfig.MySQL; my.IsConfigured() {
		warnings, err := my.Validate()

		for _, w := range warnings {
			logger.Warn().Msg(w)
		}

		if err != nil {
			logger.Error().Msgf("invalid mysql configuration: %v", err)
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
		return invalidArg("%d is not a valid value for 'appLogLineCount' argument. It should be -1 (all lines), 0 (no logs), or a positive number.", config.GlobalConfig.AppLogLineCount)
	}

	// Validate edDataFolder is not the current working directory
	if config.GlobalConfig.EdDataFolder != "" {
		currentDir, err := os.Getwd()
		if err != nil {
			return invalidArg("Failed to get current working directory: %v", err)
		}

		edDataFolderAbs, err := filepath.Abs(config.GlobalConfig.EdDataFolder)
		if err != nil {
			return invalidArg("Failed to resolve edDataFolder path '%s': %v", config.GlobalConfig.EdDataFolder, err)
		}

		currentDirAbs, err := filepath.Abs(currentDir)
		if err != nil {
			return invalidArg("Failed to resolve current directory path: %v", err)
		}

		if edDataFolderAbs == currentDirAbs {
			return invalidArg("edDataFolder cannot be the current working directory (%s). Point -edDataFolder at a different folder.", currentDirAbs)
		}
	}

	return nil
}
