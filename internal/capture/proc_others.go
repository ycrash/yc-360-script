//go:build !windows

package capture

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
)

type CIMProcess struct {
	ProcessName string
	CommandLine string
	ProcessId   int
}

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
					if len(col) > 0 {
						break
					}
				}
			}
			if len(col) > 0 {
				id := strings.TrimSpace(col[0])
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

// GetCIMProcesses is not supported on non-Windows platforms
func GetCIMProcesses(tokens config.ProcessTokens, excludes config.ProcessTokens) ([]CIMProcess, error) {
	return nil, fmt.Errorf("GetCIMProcesses is only supported on Windows platforms")
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
					if len(col) > 0 {
						break
					}
				}
			}
			if len(col) > 0 {
				id := strings.TrimSpace(col[0])
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

func GetProcessIds(tokens config.ProcessTokens, excludes config.ProcessTokens) (pids map[int]string, err error) {
	output, err := executils.CommandCombinedOutput(executils.PSGetProcessIds)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
	pids = make(map[int]string)
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		for _, exclude := range excludes {
			p := strings.Index(line, string(exclude))
			if p >= 0 {
				continue Next
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

			p := strings.Index(line, token)
			if p >= 0 {
				columns := strings.Split(line, " ")
				var col []string
				for _, column := range columns {
					if len(column) == 0 {
						continue
					}
					col = append(col, column)
					if len(col) <= 1 {
						continue
					}
					id := strings.TrimSpace(col[1])
					pid, err := strconv.Atoi(id)
					if err != nil {
						continue Next
					}
					if pid == cpid {
						continue Next
					}
					tokenPid, err := strconv.Atoi(token)
					if err == nil {
						if tokenPid != pid {
							continue Next
						}
					}
					if _, ok := pids[pid]; !ok {
						pids[pid] = appName
					}
					continue Next
				}
			}
		}
	}
	return
}
