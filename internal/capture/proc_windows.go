//go:build windows

package capture

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

func GetTopCpu() (pid int, err error) {
	output, err := executils.CommandCombinedOutput(executils.ProcessTopCPU)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		p := strings.Index(line, "java")
		if p >= 0 {
			columns := strings.Split(line, " ")
			var col []string
			for _, column := range columns {
				if len(column) > 0 {
					col = append(col, column)
					if len(col) > 6 {
						break
					}
				}
			}
			if len(col) > 6 {
				id := strings.TrimSpace(col[5])
				pid, err = strconv.Atoi(id)
				if err != nil {
					continue Next
				}
				if pid == cpid {
					continue Next
				}
				return
			}
		}
	}
	return
}

func GetTopMem() (pid int, err error) {
	output, err := executils.CommandCombinedOutput(executils.ProcessTopMEM)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		p := strings.Index(line, "java")
		if p >= 0 {
			columns := strings.Split(line, " ")
			var col []string
			for _, column := range columns {
				if len(column) > 0 {
					col = append(col, column)
					if len(col) > 6 {
						break
					}
				}
			}
			if len(col) > 6 {
				id := strings.TrimSpace(col[5])
				pid, err = strconv.Atoi(id)
				if err != nil {
					continue Next
				}
				if pid == cpid {
					continue Next
				}
				return
			}
		}
	}
	return
}

type ParsedToken struct {
	stringToken string
	intToken    int
	isIntToken  bool
	appName     string
}

type CIMProcess struct {
	ProcessName string
	CommandLine string
	ProcessId   int
}

type CIMProcessList []CIMProcess

// parseTokens parses process tokens and extracts app names
func parseTokens(tokens config.ProcessTokens) []ParsedToken {
	parsedTokens := make([]ParsedToken, 0, len(tokens))
	for _, t := range tokens {
		tokenStr := string(t)

		// Extract app name if present
		var appName string
		index := strings.Index(tokenStr, "$")
		if index >= 0 {
			// E.g: 1234$BuggyApp
			appName = tokenStr[index+1:] // e.g: BuggyApp
			tokenStr = tokenStr[:index]  // e.g: 1234
		}

		// Check if token is an integer
		intVal, err := strconv.Atoi(tokenStr)
		isIntToken := err == nil && intVal > 0

		parsedTokens = append(parsedTokens, ParsedToken{
			stringToken: tokenStr,
			intToken:    intVal,
			isIntToken:  isIntToken,
			appName:     appName,
		})
	}
	return parsedTokens
}

// ProcessWithAppName represents a CIM process with its associated app name
type ProcessWithAppName struct {
	CIMProcess
	AppName string
}

// getFilteredCIMProcesses contains the common logic for filtering processes based on tokens and excludes
// Returns both the filtered processes and their associated app names
func getFilteredCIMProcesses(tokens config.ProcessTokens, excludes config.ProcessTokens) ([]ProcessWithAppName, error) {
	output, err := executils.CommandCombinedOutput(executils.PSGetProcessIds)
	if err != nil {
		return nil, err
	}

	cimProcessList := CIMProcessList{}
	err = json.Unmarshal(output, &cimProcessList)
	if err != nil {
		return nil, err
	}

	logger.Debug().Msgf("m3_windows getFilteredCIMProcesses tokens: %v", tokens)
	logger.Debug().Msgf("m3_windows getFilteredCIMProcesses excludes: %v", excludes)
	logger.Debug().Msgf("m3_windows getFilteredCIMProcesses cimProcessList: %v", cimProcessList)

	// 1. Preprocess excludes - identify excluded processes
	excludedProcesses := make(map[int]bool)
	// exclude self Pid in case some of the cmdline args matches
	excludedProcesses[os.Getpid()] = true
	for _, cimProcess := range cimProcessList {
		for _, exclude := range excludes {
			if strings.Contains(cimProcess.CommandLine, string(exclude)) {
				excludedProcesses[cimProcess.ProcessId] = true
				break
			}
		}
	}

	// 2. Parse tokens once for performance
	parsedTokens := parseTokens(tokens)

	// 3. Process matching - collect matched processes with their app names
	matchedProcesses := make(map[int]string) // ProcessId -> AppName
	for _, token := range parsedTokens {
		for _, cimProcess := range cimProcessList {
			// Skip excluded processes
			if excludedProcesses[cimProcess.ProcessId] {
				continue
			}

			matched := false

			if token.isIntToken && cimProcess.ProcessId == token.intToken {
				// Integer token matching process ID
				matched = true
			} else if strings.Contains(cimProcess.CommandLine, token.stringToken) {
				// String token matching command line
				matched = true
			}

			if matched {
				if _, exists := matchedProcesses[cimProcess.ProcessId]; !exists {
					matchedProcesses[cimProcess.ProcessId] = token.appName
				}
			}
		}
	}

	// 4. Build result list with app names
	var result []ProcessWithAppName
	for _, cimProcess := range cimProcessList {
		if appName, matched := matchedProcesses[cimProcess.ProcessId]; matched {
			result = append(result, ProcessWithAppName{
				CIMProcess: cimProcess,
				AppName:    appName,
			})
		}
	}

	logger.Debug().Msgf("m3_windows getFilteredCIMProcesses result: %v", result)
	return result, nil
}

// GetCIMProcesses returns filtered CIM processes based on tokens and excludes
func GetCIMProcesses(tokens config.ProcessTokens, excludes config.ProcessTokens) ([]CIMProcess, error) {
	processesWithAppNames, err := getFilteredCIMProcesses(tokens, excludes)
	if err != nil {
		return nil, err
	}

	// Extract just the CIMProcess part
	result := make([]CIMProcess, len(processesWithAppNames))
	for i, processWithAppName := range processesWithAppNames {
		result[i] = processWithAppName.CIMProcess
	}

	return result, nil
}

func GetProcessIds(tokens config.ProcessTokens, excludes config.ProcessTokens) (pids map[int]string, err error) {
	processesWithAppNames, err := getFilteredCIMProcesses(tokens, excludes)
	if err != nil {
		return nil, err
	}

	pids = make(map[int]string)

	// Directly use the app names from the filtered results - no need to re-match!
	for _, processWithAppName := range processesWithAppNames {
		pids[processWithAppName.ProcessId] = processWithAppName.AppName
	}

	logger.Debug().Msgf("m3_windows GetProcessIds pids: %v", pids)
	return pids, nil
}
