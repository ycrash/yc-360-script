//go:build windows
// +build windows

package shell

type SudoHooker struct {
	PID int
}

func (s SudoHooker) Before(command Command) (result Command) {
	return command
}
