package shell

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
)

type Command []string

var NopCommand Command = nil
var SkippedNopCommandError = errors.New("skipped nop command")

type CmdHolder struct {
	*exec.Cmd
}

func (h *CmdHolder) KillAndWait() (err error) {
	if h.Cmd == nil {
		return
	}
	err = h.Cmd.Process.Kill()
	if err != nil {
		return
	}
	h.Cmd.Wait()
	return
}

func NewCommand(cmd Command, args ...string) CmdHolder {
	if len(cmd) < 1 {
		return CmdHolder{}
	}
	if len(args) > 0 {
		cmd = append(cmd, args...)
	}
	if len(cmd) == 1 {
		return CmdHolder{exec.Command(cmd[0])}
	}
	return CmdHolder{exec.Command(cmd[0], cmd[1:]...)}
}

func CommandCombinedOutput(cmd Command, args ...string) ([]byte, error) {
	c := NewCommand(cmd, args...)
	if c.Cmd == nil {
		return nil, SkippedNopCommandError
	}
	return c.CombinedOutput()
}

func CommandCombinedOutputToFile(name string, cmd Command, args ...string) (file *os.File, err error) {
	file, err = os.Create(name)
	if err != nil {
		return
	}
	c := NewCommand(cmd, args...)
	if c.Cmd == nil {
		return
	}
	output, err := c.CombinedOutput()
	if err != nil {
		return
	}
	_, err = file.Write(output)
	return
}

func CommandRun(cmd Command, args ...string) error {
	c := NewCommand(cmd, args...)
	if c.Cmd == nil {
		return nil
	}
	return c.Run()
}

func CommandStartInBackgroundWithWriter(writer io.Writer, cmd Command, args ...string) (c CmdHolder, err error) {
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, args...)
	stdout, err := c.StdoutPipe()
	if err != nil {
		return
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return
	}
	go func() {
		defer func() {
			if err != nil {
				fmt.Printf("Unexpected Error %s", err)
				os.Exit(-1)
			}
		}()
		reader := io.MultiReader(stdout, stderr)
		_, err = io.Copy(writer, reader)
		if err != nil {
			return
		}
	}()
	err = c.Start()
	return
}
