package shell

import "fmt"

type EnvHooker map[string]string

func (h EnvHooker) Before(command Command) (result Command) {
	if len(h) < 1 {
		return command
	}
	for k, v := range h {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	result = append(result, command...)
	return
}
