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

const DynamicArg = "<DynamicArg>"
const WaitCommand = "<WaitCommand>"

func (cmd *Command) AddDynamicArg(args ...string) (result Command, err error) {
	if cmd == nil {
		err = errors.New("invalid nil Command, please use NopCommand instead")
		return
	}
	if *cmd == nil {
		return NopCommand, nil
	}
	n := 0
	for _, c := range *cmd {
		if c == DynamicArg {
			n++
		}
	}
	if n != len(args) {
		return *cmd, nil
	}
	if (*cmd)[0] == WaitCommand {
		result = make(Command, 4)
		result[0] = WaitCommand
		copy(result[1:], SHELL)
	} else {
		result = make(Command, 3)
		copy(result, SHELL)
	}
	i := 0
	var command strings.Builder
	for _, c := range *cmd {
		switch c {
		case WaitCommand:
			continue
		case DynamicArg:
			command.WriteString(args[i])
			command.WriteByte(' ')
			i++
		default:
			command.WriteString(c)
			command.WriteByte(' ')
		}
	}
	result[len(result)-1] = command.String()
	return
}

var Env []string

func NewCommand(cmd Command, args ...string) CmdManager {
	if len(cmd) < 1 {
		return &WaitCmd{}
	}
	wait := cmd[0] == WaitCommand
	if wait {
		cmd = cmd[1:]
	}
	if len(args) > 0 {
		cmd = append(cmd, args...)
	}
	var command *exec.Cmd
	if len(cmd) == 1 {
		command = exec.Command(cmd[0])
	} else {
		command = exec.Command(cmd[0], cmd[1:]...)
	}
	if len(Env) > 0 {
		command.Env = Env
	}
	if wait {
		return &WaitCmd{command}
	}
	return &Cmd{WaitCmd{command}}
}

func CommandCombinedOutput(cmd Command, args ...string) ([]byte, error) {
	c := NewCommand(cmd, args...)
	if c.IsSkipped() {
		return nil, SkippedNopCommandError
	}
	return c.CombinedOutput()
}

func CommandCombinedOutputToWriter(writer io.Writer, cmd Command, args ...string) (err error) {
	c := NewCommand(cmd, args...)
	if c.IsSkipped() {
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
	if c.IsSkipped() {
		return nil
	}
	return c.Run()
}

func CommandStartInBackground(cmd Command, args ...string) (c CmdManager, err error) {
	c = &WaitCmd{}
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, args...)
	if c.IsSkipped() {
		return
	}
	err = c.Start()
	return
}

func CommandStartInBackgroundToWriter(writer io.Writer, cmd Command, args ...string) (c CmdManager, err error) {
	c = &WaitCmd{}
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, args...)
	if c.IsSkipped() {
		return
	}
	c.SetStdoutAndStderr(writer)
	err = c.Start()
	return
}

func CommandStartInBackgroundToFile(name string, cmd Command, args ...string) (file *os.File, c CmdManager, err error) {
	c = &WaitCmd{}
	file, err = os.Create(name)
	if err != nil {
		return
	}
	c, err = CommandStartInBackgroundToWriter(file, cmd, args...)
	if err != nil || c.IsSkipped() {
		file.Close()
		file = nil
	}
	return
}
