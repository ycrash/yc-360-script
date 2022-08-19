package shell

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"shell/logger"
	"strings"
	"sync"
	"time"
)

type Command []string

var NopCommand Command = nil
var SkippedNopCommandError = errors.New("skipped nop command")

const DynamicArg = "<DynamicArg>"
const WaitCommand = "<WaitCommand>"

func Append(c Command, args ...string) Command {
	if len(c) < 1 {
		return NopCommand
	}
	return append(c, args...)
}

func (cmd *Command) AddDynamicArg(args ...string) (result Command, err error) {
	if cmd == nil {
		err = errors.New("invalid nil Command, please use NopCommand instead")
		return
	}
	if len(*cmd) < 1 {
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

func (cmd *Command) addDynamicArg(args ...string) (result Command, err error) {
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
		result = make(Command, 0, len(*cmd)+1)
		result = append(result, WaitCommand)
	} else {
		result = make(Command, 0, len(*cmd))
	}
	i := 0
	for _, c := range *cmd {
		switch c {
		case WaitCommand:
			continue
		case DynamicArg:
			result = append(result, args[i])
			i++
		default:
			result = append(result, c)
		}
	}
	return
}

var Env []string

func NewCommand(cmd Command, hookers ...Hooker) CmdManager {
	if len(cmd) < 1 {
		return &WaitCmd{}
	}
	wait := cmd[0] == WaitCommand
	if wait {
		cmd = cmd[1:]
	}
	for _, hooker := range hookers {
		cmd = hooker.Before(cmd)
	}
	var command *exec.Cmd
	if len(cmd) == 1 {
		command = exec.Command(cmd[0])
	} else {
		command = exec.Command(cmd[0], cmd[1:]...)
	}
	if len(Env) > 0 {
		command.Env = os.Environ()
		command.Env = append(command.Env, Env...)
	}
	for _, hooker := range hookers {
		hooker.After(command)
	}
	if wait {
		return &WaitCmd{command}
	}
	return &Cmd{WaitCmd{command}}
}

func CommandCombinedOutput(cmd Command, hookers ...Hooker) ([]byte, error) {
	c := NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return nil, SkippedNopCommandError
	}
	return c.CombinedOutput()
}

func CommandCombinedOutputToWriterNoTimeout(writer io.Writer, cmd Command, hookers ...Hooker) (err error) {
	c := NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return
	}
	output, err := c.CombinedOutput()
	if err != nil {
		if len(output) > 1 {
			err = fmt.Errorf("%w because %s", err, output)
		}
		if _, e := writer.Write(output); e != nil {
			err = fmt.Errorf("%w and %s", err, e.Error())
		}
		return
	}
	_, err = writer.Write(output)
	return
}

func CommandCombinedOutputToWriter(writer io.Writer, cmd Command, hookers ...Hooker) (err error) {
	c := NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return
	}

	channelDone := make(chan bool)
	go func() {
		// --- execution routine start
		var output []byte
		output, err = c.CombinedOutput()
		if err != nil {
			if len(output) > 1 {
				err = fmt.Errorf("%w because %s", err, output)
			}
			if _, e := writer.Write(output); e != nil {
				err = fmt.Errorf("%w and %s", err, e.Error())
			}
		} else {
			_, err = writer.Write(output)
		}
		// --- execution routine finish
		channelDone <- true
	}()

	// execution timer
	timerDuration := 1 * time.Minute
	timer := time.NewTimer(timerDuration)
	logger.Log("Timer created for the command execution (%ds) [%s]", int(timerDuration/time.Second), c.String())
	select {
	case <-timer.C:
		logger.Log("Timeout happened during the command execution [%s]", c.String())
		err = c.Kill()
		if err != nil {
			_ = fmt.Errorf("Error doing cmd.Kill() invocation: " + err.Error())
		}
	case <-channelDone:
		timer.Stop()
		logger.Log("Command executed without timeout [%s]", c.String())
	}
	return
}

func CommandCombinedOutputToFile(name string, cmd Command, hookers ...Hooker) (file *os.File, err error) {
	file, err = os.Create(name)
	if err != nil {
		return
	}
	err = CommandCombinedOutputToWriter(file, cmd, hookers...)
	if err != nil {
		_ = file.Close()
		file = nil
	}
	return
}

func CommandRun(cmd Command, hookers ...Hooker) error {
	c := NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return nil
	}
	return c.Run()
}

func CommandStartInBackground(cmd Command, hookers ...Hooker) (c CmdManager, err error) {
	c = &WaitCmd{}
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return
	}
	err = c.Start()
	return
}

func CommandStartInBackgroundToWriter(writer io.Writer, cmd Command, hookers ...Hooker) (c CmdManager, err error) {
	c = &WaitCmd{}
	if len(cmd) < 1 {
		return
	}
	c = NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return
	}
	c.SetStdoutAndStderr(writer)
	err = c.Start()
	return
}

func CommandStartInBackgroundToFile(name string, cmd Command, hookers ...Hooker) (file *os.File, c CmdManager, err error) {
	c = &WaitCmd{}
	file, err = os.Create(name)
	if err != nil {
		return
	}
	c, err = CommandStartInBackgroundToWriter(file, cmd, hookers...)
	if err != nil || c.IsSkipped() {
		_ = file.Close()
		file = nil
	}
	return
}

func RunCaptureCmd(pid int, cmd string) (output []byte, err error) {
	Env = []string{fmt.Sprintf("pid=%d", pid)}
	output, err = CommandCombinedOutput(append(SHELL, cmd))
	logger.Log(`run capture cmd: %s
pid: %d
result: %s
err: %v
`, cmd, pid, output, err)
	return
}

var workDir string

func init() {
	workDir, _ = os.Getwd()
}

func Executable() (path string) {
	path, err := os.Executable()
	if err != nil {
		path = filepath.Join(workDir, os.Args[0])
		logger.Warn().Err(err).Str("path", path).Msg("Failed to get executable path")
	}
	return
}

var tmpPathMtx sync.RWMutex
var tmpPath string

func Copy2TempPath() (name string, err error) {
	tmpPathMtx.RLock()
	name = tmpPath
	tmpPathMtx.RUnlock()

	if len(name) > 0 {
		return
	}

	tmpPathMtx.Lock()
	defer tmpPathMtx.Unlock()

	path := Executable()
	if len(path) < 1 {
		err = errors.New("failed to Copy2TempPath: invalid path")
		return
	}
	in, err := os.Open(path)
	if err != nil {
		return
	}
	file, err := os.CreateTemp("", "yc")
	if err != nil {
		return
	}
	defer func() {
		err := file.Close()
		if err != nil {
			logger.Warn().Err(err).Msg("failed to close file in Copy2TempPath")
		}
	}()
	_, err = io.Copy(file, in)
	if err != nil {
		return
	}
	err = file.Chmod(0777)
	name = file.Name()
	tmpPath = name
	return
}

func RemoveFromTempPath() {
	tmpPathMtx.RLock()
	name := tmpPath
	tmpPathMtx.RUnlock()

	if len(name) > 0 {
		os.Remove(name)
	}
}
