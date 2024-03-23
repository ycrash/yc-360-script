//go:build windows
// +build windows

package internal

import (
	"encoding/json"
	"strconv"
	"strings"

	"shell/internal/config"
	"shell/internal/logger"
)

type CIMProcess struct {
	ProcessName string
	CommandLine string
	ProcessId   int
}

type CIMProcessList []CIMProcess

func GetProcessIds(tokens config.ProcessTokens, excludes config.ProcessTokens) (pids map[int]string, err error) {
	output, err := CommandCombinedOutput(M3PS)
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
