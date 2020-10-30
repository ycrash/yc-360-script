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

func GetProcessIds(tokens config.ProcessTokens) (pids []int, err error) {
	output, err := CommandCombinedOutput(M3PS)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		for _, token := range tokens {
			p := strings.Index(line, string(token))
			if p >= 0 {
				columns := strings.Split(line, " ")
				var col []string
				for _, column := range columns {
					if len(column) > 0 {
						col = append(col, column)
						if len(col) > 2 {
							break
						}
					}
				}
				if len(col) > 2 {
					id := col[1]
					pid, err := strconv.Atoi(id)
					if err != nil {
						continue Next
					}
					if pid == cpid {
						continue Next
					}
					pids = append(pids, pid)
					continue Next
				}
			}
		}
	}
	return
}
