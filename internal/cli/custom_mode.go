package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/procps"
	"yc-agent/internal/capture/ycattach"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// runRawCaptureModeIfConditionSatisfied runs custom capture mode depending on the args.
// Raw capture mode is capture mode that accepts arbitrary arguments, so it should be run
// before parsing config flags.
// For example: -topMode accepts -bc or -bH that will be passed through to the underlying top program.
func runRawCaptureModeIfConditionSatisfied() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "-vmstatMode":
			ret := procps.VMStat(os.Args[1:]...)
			os.Exit(ret)
		case "-topMode":
			ret := procps.Top(append([]string{"top"}, os.Args[2:]...)...)
			os.Exit(ret)
		}
	}
}

// runCaptureModeIfConditionSatisfied runs capture mode depending on the config.
// Capture mode can be run after parsing the config flags.
func runCaptureModeIfConditionSatisfied() {
	if config.GlobalConfig.TestCIMMode {
		runTestCIMMode()
		os.Exit(0)
	}
	if config.GlobalConfig.GCCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureGCLog(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.TDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.CaptureThreadDump(pid)
		os.Exit(ret)
	}
	if config.GlobalConfig.HDCaptureMode {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		if len(config.GlobalConfig.HeapDumpPath) <= 0 {
			logger.Log("-hdPath can not be empty")
			os.Exit(1)
		}
		ret := ycattach.CaptureHeapDump(pid, config.GlobalConfig.HeapDumpPath)
		os.Exit(ret)
	}
	if len(config.GlobalConfig.JCmdCaptureMode) > 0 {
		pid, err := strconv.Atoi(config.GlobalConfig.Pid)
		if err != nil {
			logger.Log("invalid -p %s", config.GlobalConfig.Pid)
			os.Exit(1)
		}
		ret := ycattach.Capture(pid, "jcmd", config.GlobalConfig.JCmdCaptureMode)
		os.Exit(ret)
	}
}

// runTestCIMMode tests the GetCIMProcesses function and prints the results
func runTestCIMMode() {
	fmt.Println("Testing GetCIMProcesses function...")
	fmt.Printf("Process tokens: %v\n", config.GlobalConfig.ProcessTokens)
	fmt.Printf("Exclude tokens: %v\n", config.GlobalConfig.ExcludeProcessTokens)
	fmt.Println()

	processes, err := capture.GetCIMProcesses(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	if err != nil {
		fmt.Printf("Error calling GetCIMProcesses: %v\n", err)
		return
	}

	fmt.Printf("Found %d matching processes:\n", len(processes))
	if len(processes) == 0 {
		fmt.Println("No processes found matching the criteria.")
		return
	}

	// Print results in JSON format for better readability
	jsonData, err := json.MarshalIndent(processes, "", "  ")
	if err != nil {
		fmt.Printf("Error marshaling to JSON: %v\n", err)
		// Fallback to simple format
		for i, process := range processes {
			fmt.Printf("%d. ProcessId: %d, ProcessName: %s, CommandLine: %s\n",
				i+1, process.ProcessId, process.ProcessName, process.CommandLine)
		}
	} else {
		fmt.Println(string(jsonData))
	}
}
