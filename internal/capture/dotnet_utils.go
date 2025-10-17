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
	toolPath := config.GlobalConfig.DotnetToolPath
	if toolPath == "" {
		return nil, fmt.Errorf("dotnet tool path not configured")
	}

	// Build the command: [toolPath, args...]
	cmdArgs := append([]string{toolPath}, args...)

	logger.Log("Executing dotnet tool: %v", cmdArgs)

	// Execute the command and capture output to file
	cmd, err := executils.CommandStartInBackground(cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("failed to start dotnet tool %v: %w", cmdArgs, err)
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
