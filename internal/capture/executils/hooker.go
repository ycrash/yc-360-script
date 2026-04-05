package executils

import (
	"os/exec"
)

type Hooker interface {
	Before(Command) Command
	After(command *exec.Cmd)
}

// DirHooker sets the working directory of the spawned process.
type DirHooker struct {
	Dir string
}

func (h DirHooker) Before(cmd Command) Command { return cmd }
func (h DirHooker) After(cmd *exec.Cmd)        { cmd.Dir = h.Dir }
