// +build windows

package shell

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	"shell/config"
)

func GetProcessIds(tokens config.ProcessTokens) (pids []int, err error) {
	arg := "("
	for i, token := range tokens {
		arg += "Commandline like '%" + string(token) + "%' "
		if i != len(tokens)-1 {
			arg += "or "
		}
	}
	arg += ") "
	arg += "and Name != 'WMIC.exe'"
	command, err := M3PS.addDynamicArg(arg)
	if err != nil {
		return
	}
	output, err := CommandCombinedOutput(command)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		columns := strings.Split(line, " ")
		var col []string
		for _, column := range columns {
			if len(column) > 0 {
				col = append(col, column)
				if len(col) > 1 {
					break
				}
			}
		}
		if len(col) > 1 {
			id := strings.TrimSpace(col[1])
			pid, err := strconv.Atoi(id)
			if err != nil {
				continue Next
			}
			pids = append(pids, pid)
			continue Next
		}
	}
	return
}
