//go:build windows
// +build windows

package shell

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"

	"shell/config"
)

func GetProcessIds(tokens config.ProcessTokens, excludes config.ProcessTokens) (pids map[int]string, err error) {
	arg := "Name != 'WMIC.exe'"
	command, err := M3PS.addDynamicArg(arg)
	if err != nil {
		return
	}
	output, err := CommandCombinedOutput(command)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	pids = make(map[int]string)
	if !scanner.Scan() {
		err = errors.New("no line found in the commandline result")
		return
	}
	header := scanner.Text()
	header = strings.TrimSpace(header)
	indexProcessId := strings.Index(header, "ProcessId")

	if indexProcessId < 0 {
		err = errors.New("string ProcessId not found in the header line")
		return
	}

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

		if len(line) <= indexProcessId {
			continue Next
		}

		wmicProcessId := strings.TrimSpace(line[indexProcessId:])

		for _, t := range tokens {
			token := string(t)
			var appName string
			index := strings.Index(token, "$")
			if index >= 0 {
				appName = token[index+1:]
				token = token[:index]
			}

			if token == wmicProcessId {
				pid, err := strconv.Atoi(wmicProcessId)
				if err != nil {
					continue Next
				}

				if _, ok := pids[pid]; !ok {
					pids[pid] = appName
				}
				continue Next
			}
		}
	}
	return
}
