package capture

import (
	"fmt"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// executeDotnetTool runs the yc-360-tool-dotnet executable with the given arguments
// and captures the output to a file. Returns the file handle and any error.
func executeDotnetTool(args []string, outputPath string) (*os.File, error) {
	// Get the resolved tool path from configuration
	toolPath := config.GlobalConfig.DotnetToolPath
	if toolPath == "" {
		return nil, fmt.Errorf("dotnet tool path not configured")
	}

	// Create output file
	file, err := os.Create(outputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	// Build the command: [toolPath, args...]
	cmdArgs := append([]string{toolPath}, args...)

	logger.Log("Executing dotnet tool: %v", cmdArgs)

	// Execute the command and capture output to file
	cmd, err := executils.CommandStartInBackgroundToWriter(file, cmdArgs)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to start dotnet tool: %w", err)
	}

	// Wait for command to complete
	err = cmd.Wait()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("dotnet tool execution failed: %w", err)
	}

	// Check exit code
	if cmd.ExitCode() != 0 {
		file.Close()
		return nil, fmt.Errorf("dotnet tool exited with code %d", cmd.ExitCode())
	}

	// Sync the file to ensure all data is written
	if err := file.Sync(); err != nil {
		logger.Log("failed to sync file: %v", err)
	}

	return file, nil
}
