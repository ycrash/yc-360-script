//go:build windows
// +build windows

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

func GetProcessIds(tokens config.ProcessTokens, excludes config.ProcessTokens) (pids map[int]string, err error) {
	output, err := executils.CommandCombinedOutput(executils.M3PS)
	if err != nil {
		return
	}

	cimProcessList := CIMProcessList{}
	err = json.Unmarshal(output, &cimProcessList)
	if err != nil {
		return
	}

	pids = map[int]string{}

	logger.Debug().Msgf("m3_windows GetProcessIds tokens: %v", tokens)
	logger.Debug().Msgf("m3_windows GetProcessIds excludes: %v", excludes)
	logger.Debug().Msgf("m3_windows GetProcessIds cimProcessList: %v", cimProcessList)

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

	// 2. Preprocess tokens - parse once, before entering loop for performance consideration
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

	// 3. Process matching
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
				if _, exists := pids[cimProcess.ProcessId]; !exists {
					pids[cimProcess.ProcessId] = token.appName
				}
			}
		}
	}

	logger.Debug().Msgf("m3_windows GetProcessIds pids: %v", pids)
	return
}
