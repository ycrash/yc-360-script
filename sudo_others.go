//go:build !windows
// +build !windows

package shell

import (
	"bufio"
	"fmt"
	"os"
	"shell/logger"
	"strconv"
	"strings"
)

type SudoHooker struct {
	PID int
}

func (s SudoHooker) Before(command Command) (result Command) {
	status, err := os.Open(fmt.Sprintf("/proc/%d/status", s.PID))
	if err != nil {
		return command
	}
	defer func() {
		_ = status.Close()
	}()
	scanner := bufio.NewScanner(status)
	var uid string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Uid:") {
			cols := strings.Split(line, "\t")
			if len(cols) > 1 {
				uid = strings.TrimSpace(cols[1])
			}
			break
		}
	}
	if len(uid) < 1 {
		return command
	}
	id, err := strconv.Atoi(uid)
	if err != nil {
		return command
	}
	if id == os.Getuid() {
		return command
	}
	_, err = os.Stat("/usr/bin/sudo")
	if err != nil {
		return command
	}
	result = append(Command{"sudo", "-u", fmt.Sprintf("#%s", uid)}, command...)
	logger.Info().Str("cmd", strings.Join(result, " ")).Msg("sudo hooker result")
	return
}
