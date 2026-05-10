package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// dotnetToolFriendlyError defines a mapping from an error substring to a
// user-friendly message that helps the operator resolve the problem.
type dotnetToolFriendlyError struct {
	substring string
	message   string
}

// knownDotnetToolErrors lists recognised error patterns and the guidance
// that should be shown to the user alongside the original error message.
var knownDotnetToolErrors = []dotnetToolFriendlyError{
	{
		substring: "requires elevation",
		message:   "administrator privileges are required. Please re-run the command from an elevated Command Prompt or PowerShell (Run as Administrator)",
	},
}

const DotnetSourceUserOverride = "user-override"
const DotnetSourceArchMatched = "arch-matched"
const DotnetSourceDefault = "default"

type DotnetToolResolution struct {
	Path   string
	Source string // "user-override", "arch-matched", "default"
}

// wrapDotnetToolStartError wraps a command-start error, appending a
// user-friendly message when the error matches a known pattern. The original
// error message is always preserved for debugging.
func wrapDotnetToolStartError(err error, cmdArgs []string) error {
	msg := err.Error()
	for _, known := range knownDotnetToolErrors {
		if strings.Contains(msg, known.substring) {
			return fmt.Errorf("failed to start dotnet tool %v: %s\nOriginal error: %w", cmdArgs, known.message, err)
		}
	}
	return fmt.Errorf("failed to start dotnet tool %v: %w", cmdArgs, err)
}

func resolveDotnetToolForPid(pid int) (DotnetToolResolution, error) {
	// user override
	if config.GlobalConfig.DotnetToolPath != "" {
		resolvedPath, err := config.ResolveDotnetToolOverride()
		if err != nil {
			return DotnetToolResolution{}, err
		}
		return DotnetToolResolution{Path: resolvedPath, Source: DotnetSourceUserOverride}, nil
	}

	// arch matched
	arch, detectErr := detectTargetArch(pid)
	if detectErr != nil {
		logger.Warn().Err(detectErr).Int("pid", pid).Msg("could not detect target arch")
	}
	if arch != "" {
		name := config.DotnetToolNameForArch(arch)
		if p, ok := config.FindDotnetToolNearYcOrPath(name); ok {
			return DotnetToolResolution{Path: p, Source: DotnetSourceArchMatched}, nil
		}

		return DotnetToolResolution{}, fmt.Errorf(".NET tool for PID %d (%s) not found. expected %s next to yc or on PATH", pid, arch, name)
	}

	// No arch info — fall back to default tool name
	if p, ok := config.FindDotnetToolNearYcOrPath(config.DefaultDotnetToolName); ok {
		if detectErr != nil {
			logger.Warn().
				Err(detectErr).
				Int("pid", pid).
				Str("path", p).
				Msg("using legacy .NET tool path because target arch detection failed")
		}
		return DotnetToolResolution{Path: p, Source: DotnetSourceDefault}, nil
	}

	if detectErr != nil {
		return DotnetToolResolution{}, fmt.Errorf(
			".NET tool path %q not found near yc or on PATH (target arch detection for PID %d failed: %w)",
			config.DefaultDotnetToolName, pid, detectErr)
	}
	return DotnetToolResolution{}, fmt.Errorf(
		".NET tool path %q not found near yc or on PATH", config.DefaultDotnetToolName)
}

// executeDotnetTool runs the configured .NET tool executable with the given arguments
// and captures the output to a file. Returns the file handle and any error.
func executeDotnetTool(pid int, args []string, outputPath string) (*os.File, error) {
	toolResolution, err := resolveDotnetToolForPid(pid)
	if err != nil {
		return nil, err
	}

	// Build the command: [toolPath, args...]
	cmdArgs := append([]string{toolResolution.Path}, args...)

	logger.Log("Executing dotnet tool: %v", cmdArgs)

	// Execute the command and capture output to file
	cmd, err := executils.CommandStartInBackground(cmdArgs)
	if err != nil {
		return nil, wrapDotnetToolStartError(err, cmdArgs)
	}

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("dotnet tool execution failed %v: %w", cmdArgs, err)
	}

	// Check exit code
	if cmd.ExitCode() != 0 {
		return nil, fmt.Errorf("dotnet tool %v exited with code %d", cmdArgs, cmd.ExitCode())
	}

	// Validate output file exists
	fileInfo, err := os.Stat(outputPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("dotnet tool %v completed but expected output file %s was not created", cmdArgs, outputPath)
		}
		return nil, fmt.Errorf("failed to check output file %s: %w", outputPath, err)
	}

	///// going to change the artifacts log name if the mode is "onlyCapture"
	if config.GlobalConfig.OnlyCapture {
		logger.Log("onlyCapture mode detected. renaming .NET artifacts")
		fileName := filepath.Base(outputPath)
		dir := filepath.Dir(outputPath)
		if strings.HasPrefix(fileName, "gc") {
			newPath := filepath.Join(dir, "gc.log")

			err = os.Rename(outputPath, newPath)
			if err != nil {
				return nil, fmt.Errorf("failed to rename file from %s to %s: %w", outputPath, newPath, err)
			}

			// Update outputPath if you use it later
			outputPath = newPath
		} else if strings.HasPrefix(fileName, "thread") {
			newPath := filepath.Join(dir, "threaddump.out")

			err = os.Rename(outputPath, newPath)
			if err != nil {
				return nil, fmt.Errorf("failed to rename file from %s to %s: %w", outputPath, newPath, err)
			}

			// Update outputPath if you use it later
			outputPath = newPath
		} else if strings.HasPrefix(fileName, "heap") {
			newPath := filepath.Join(dir, "hdsub.out")

			err = os.Rename(outputPath, newPath)
			if err != nil {
				return nil, fmt.Errorf("failed to rename file from %s to %s: %w", outputPath, newPath, err)
			}

			// Update outputPath if you use it later
			outputPath = newPath
		}
	}

	// Validate file has content
	if fileInfo.Size() == 0 {
		return nil, fmt.Errorf("dotnet tool %v created empty output file %s", cmdArgs, outputPath)
	}

	// Open expected output
	file, err := os.Open(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open output file %s: %w", outputPath, err)
	}

	return file, nil
}

// startDotnetToolInBackground starts the configured .NET tool executable with the
// given arguments and returns the running command handle without waiting.
func startDotnetToolInBackground(pid int, args []string, hookers ...executils.Hooker) (executils.CmdManager, error) {
	toolResolution, err := resolveDotnetToolForPid(pid)
	if err != nil {
		return nil, err
	}

	cmdArgs := append([]string{toolResolution.Path}, args...)
	logger.Log("Starting dotnet tool in background: %v", cmdArgs)

	cmd, err := executils.CommandStartInBackground(cmdArgs, hookers...)
	if err != nil {
		return nil, wrapDotnetToolStartError(err, cmdArgs)
	}

	return cmd, nil
}
