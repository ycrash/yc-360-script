package executils

import (
	"fmt"
	"os/exec"
)

type EnvHooker map[string]string

func (h EnvHooker) After(command *exec.Cmd) {
	for k, v := range h {
		command.Env = append(command.Env, fmt.Sprintf("%s=%s", k, v))
	}
}

func (h EnvHooker) Before(command Command) (result Command) {
	return command
}
