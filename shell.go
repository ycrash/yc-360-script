package shell

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

type Command []string

var NopCommand Command = nil
var SkippedNopCommandError = errors.New("skipped nop command")

var DynamicArg = "<DynamicArg>"

func (cmd *Command) AddDynamicArg(args ...string) (result Command, err error) {
	if *cmd == nil {
		return NopCommand, nil
	}

	if cmd == nil {
		err = errors.New("invalid nil Command, please use NopCommand instead")
		return
	}
	n := 0
	for _, c := range *cmd {
		if c == DynamicArg {
			n++
		}
	}
	if n != len(args) {
		err = errors.New("invalid num of args")
		return
	}
	result = make(Command, 3)
	copy(result, shell)
	i := 0
	var command strings.Builder
	for _, c := range *cmd {
		if c == DynamicArg {
			command.WriteString(args[i])
			command.WriteByte(' ')
			i++
		} else {
			command.WriteString(c)
			command.WriteByte(' ')
		}
	}
	result[2] = command.String()
	return
}

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
	_ = h.Cmd.Wait()
	return
}

func (h *CmdHolder) IsSkipped() bool {
	if h.Cmd == nil {
		return true
	}
	return false
}

func (h *CmdHolder) Wait() {
	if h.Cmd == nil {
		return
	}
	_ = h.Cmd.Wait()
}

func (h *CmdHolder) Interrupt() (err error) {
	if h.Cmd == nil {
		return
	}
	err = h.Cmd.Process.Signal(os.Interrupt)
	return
}

func (h *CmdHolder) Kill() (err error) {
	if h.Cmd == nil {
		return
	}
	err = h.Cmd.Process.Kill()
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

func CommandCombinedOutputToWriter(writer io.Writer, cmd Command, args ...string) (err error) {
	c := NewCommand(cmd, args...)
	if c.Cmd == nil {
		return
	}
	output, err := c.CombinedOutput()
	if err != nil {
		if len(output) > 1 {
			err = fmt.Errorf("%w because %s", err, output)
		}
		return
	}
	_, err = writer.Write(output)
	return
}

func CommandCombinedOutputToFile(name string, cmd Command, args ...string) (file *os.File, err error) {
	file, err = os.Create(name)
	if err != nil {
		return
	}
	err = CommandCombinedOutputToWriter(file, cmd, args...)
	if err != nil {
		file.Close()
		file = nil
	}
	return
}

func CommandRun(cmd Command, args ...string) error {
	c := NewCommand(cmd, args...)
	if c.Cmd == nil {
		return nil
	}
	return c.Run()
}

func CommandStartInBackground(cmd Command, args ...string) (c CmdHolder, err error) {
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, args...)
	err = c.Start()
	return
}

func CommandStartInBackgroundToWriter(writer io.Writer, cmd Command, args ...string) (c CmdHolder, err error) {
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, args...)
	c.Stdout = writer
	c.Stderr = writer
	err = c.Start()
	return
}

func CommandStartInBackgroundToFile(name string, cmd Command, args ...string) (file *os.File, c CmdHolder, err error) {
	file, err = os.Create(name)
	if err != nil {
		return
	}
	c, err = CommandStartInBackgroundToWriter(file, cmd, args...)
	if err != nil {
		file.Close()
		file = nil
	}
	return
}
