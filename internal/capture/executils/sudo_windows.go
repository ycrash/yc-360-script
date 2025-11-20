//go:build windows

package executils

import "os/exec"

type SudoHooker struct {
	PID int
}

func (s SudoHooker) After(command *exec.Cmd) {
}

func (s SudoHooker) Before(command Command) (result Command) {
	return command
}
