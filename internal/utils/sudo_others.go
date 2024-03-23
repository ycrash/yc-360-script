//go:build !windows
// +build !windows

package utils

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"shell/internal/logger"
)

type SudoHooker struct {
	PID int
}

func (s SudoHooker) After(command *exec.Cmd) {
}

func (s SudoHooker) Before(command Command) (result Command) {
	uid, err := GetUid(s.PID)
	if err != nil || len(uid) < 1 {
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
	result = append(Command{"sudo", "-E", "-u", fmt.Sprintf("#%s", uid)}, command...)
	logger.Info().Str("cmd", strings.Join(result, " ")).Msg("sudo hooker result")
	return
}

func GetUid(pid int) (uid string, err error) {
	status, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return
	}
	defer func() {
		_ = status.Close()
	}()
	scanner := bufio.NewScanner(status)
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
	return
}
