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

NextProcess:
	for _, cimProcess := range cimProcessList {
		for _, exclude := range excludes {
			if strings.Contains(cimProcess.CommandLine, string(exclude)) {
				continue NextProcess
			}
		}

		for _, t := range tokens {
			token := string(t)
			var appName string
			index := strings.Index(token, "$")
			if index >= 0 {
				appName = token[index+1:]
				token = token[:index]
			}

			if cimProcessContainsToken(cimProcess, token) {
				if _, ok := pids[cimProcess.ProcessId]; !ok {
					pids[cimProcess.ProcessId] = appName
				}
				continue NextProcess
			}
		}
	}
	logger.Debug().Msgf("m3_windows GetProcessIds pids: %v", pids)

	return
}

func cimProcessContainsToken(cimProcess CIMProcess, token string) bool {
	if strings.Contains(cimProcess.CommandLine, token) {
		return true
	} else {
		// token can be an int
		tokenInt, _ := strconv.Atoi(token)
		if tokenInt > 0 && cimProcess.ProcessId == tokenInt {
			return true
		}
	}

	return false
}
