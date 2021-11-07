//go:build !windows
// +build !windows

package shell

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"

	"shell/config"
)

func GetProcessIds(tokens config.ProcessTokens) (pids map[int]string, err error) {
	output, err := CommandCombinedOutput(M3PS)
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
					if len(column) <= 0 {
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
