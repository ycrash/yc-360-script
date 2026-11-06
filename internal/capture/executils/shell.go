package executils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

type Command []string

var NopCommand Command = nil
var ErrSkippedNopCommandError = errors.New("skipped nop command")

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

// ExpandDynamicArgs returns a copy of cmd with every DynamicArg placeholder
// replaced by the matching value from args, keeping the command as a plain
// argv slice.
func (cmd Command) ExpandDynamicArgs(args ...string) (Command, error) {
	if len(cmd) < 1 {
		return NopCommand, nil
	}

	n := 0
	for _, c := range cmd {
		n += strings.Count(c, DynamicArg)
	}
	if n != len(args) {
		return nil, fmt.Errorf("command has %d dynamic arg placeholders but got %d args", n, len(args))
	}

	result := make(Command, len(cmd))
	i := 0
	for j, c := range cmd {
		// Walk the original text: rescanning would expand a placeholder that
		// arrived inside an arg and run off the end of args.
		var expanded strings.Builder
		for {
			k := strings.Index(c, DynamicArg)
			if k < 0 {
				expanded.WriteString(c)
				break
			}
			expanded.WriteString(c[:k])
			expanded.WriteString(args[i])
			i++
			c = c[k+len(DynamicArg):]
		}
		result[j] = expanded.String()
	}

	return result, nil
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
		return nil, ErrSkippedNopCommandError
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

// ErrCommandTimeout is returned (wrapped) by CommandCombinedOutputToWriter
// and CommandCombinedOutputToFile when a command ran longer than its time
// limit and was stopped. Its output is discarded, so callers fall back to
// their next way of collecting the data.
var ErrCommandTimeout = errors.New("command timed out")

// RunTime is a Hooker for a command that runs for a set time by design, such
// as jstat taking 30 samples 2 seconds apart. Its time limit is that run time
// plus -cmdTimeout, instead of -cmdTimeout alone.
type RunTime time.Duration

func (RunTime) Before(cmd Command) Command { return cmd }
func (RunTime) After(*exec.Cmd)            {}

// CmdTimeout is -cmdTimeout, or 60s when it is not set.
func CmdTimeout() time.Duration {
	if d := config.GlobalConfig.CmdTimeout.Duration(); d > 0 {
		return d
	}
	return 60 * time.Second
}

func runTimeOf(hookers []Hooker) (total time.Duration) {
	for _, h := range hookers {
		if rt, ok := h.(RunTime); ok {
			total += time.Duration(rt)
		}
	}
	return total
}

// CommandCombinedOutputToWriter runs cmd and writes its combined stdout and
// stderr to writer once it exits. A command that runs longer than
// -cmdTimeout (plus its RunTime, if any) is stopped, nothing is written, and
// an ErrCommandTimeout error is returned.
func CommandCombinedOutputToWriter(writer io.Writer, cmd Command, hookers ...Hooker) (err error) {
	c := NewCommand(cmd, hookers...)
	if c.IsSkipped() {
		return
	}

	// The output goes to a buffer that only this function reads, and only
	// after the command exits, so a stopped command can't write into writer
	// after we return. One buffer for both streams, as CombinedOutput does.
	var output bytes.Buffer
	c.SetStdoutAndStderr(&output)
	if err = c.Start(); err != nil {
		return err
	}

	done := make(chan error, 1) // buffered: Wait never blocks, even after a timeout
	go func() { done <- c.Wait() }()

	runTime := runTimeOf(hookers)
	limit := runTime + CmdTimeout()
	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case err = <-done:
	case <-timer.C:
		killErr := stopProcess(c)
		finished := false
		if errors.Is(killErr, os.ErrProcessDone) {
			// It exited just as the limit was reached: use its result if
			// Wait returns promptly (it may not, if a child process it
			// started still holds the output pipe).
			select {
			case err = <-done:
				finished = true
			case <-time.After(time.Second):
			}
		} else if killErr != nil {
			logger.Warn().Msgf("Could not stop %s after it timed out: %s", commandName(cmd), killErr)
		}
		if !finished {
			logger.Warn().Msg(timeoutMessage(cmd, runTime, CmdTimeout(), c.String()))
			return fmt.Errorf("%w: %s ran longer than %s", ErrCommandTimeout, commandName(cmd), limit)
		}
	}

	if err != nil {
		if output.Len() > 1 {
			err = fmt.Errorf("%w because %s", err, output.Bytes())
		}
		if _, e := writer.Write(output.Bytes()); e != nil {
			err = fmt.Errorf("%w and %s", err, e.Error())
		}
		return err
	}
	_, err = writer.Write(output.Bytes())
	return err
}

// stopProcess kills a command that timed out. Kill is a no-op for a
// WaitCommand (it is meant to run to the end), but past its time limit it
// has to stop too.
func stopProcess(c CmdManager) error {
	var p *os.Process
	switch v := c.(type) {
	case *Cmd:
		if v.Cmd != nil {
			p = v.Process
		}
	case *WaitCmd:
		if v.Cmd != nil {
			p = v.Process
		}
	}
	if p == nil {
		return nil
	}
	return p.Kill()
}

// timeoutMessage explains a command stopped at its time limit, e.g. "jstat
// was stopped because it ran longer than -cmdTimeout (1s), ...".
func timeoutMessage(cmd Command, runTime, cmdTimeout time.Duration, commandLine string) string {
	limit := fmt.Sprintf("-cmdTimeout (%s)", cmdTimeout)
	if runTime > 0 {
		limit = fmt.Sprintf("its %s run time plus -cmdTimeout (%s)", runTime, cmdTimeout)
	}
	return fmt.Sprintf("%s was stopped because it ran longer than %s, so its data was not captured. Increase -cmdTimeout to give it more time. Command: %s",
		commandName(cmd), limit, commandLine)
}

// commandName returns the program's short name, e.g. "jstat" for
// "D:\JDK\jdk-17\bin\jstat" or "tasklist" for "C:\WINDOWS\system32\tasklist.exe".
func commandName(cmd Command) string {
	if len(cmd) > 0 && cmd[0] == WaitCommand {
		cmd = cmd[1:] // a marker, not the program
	}
	if len(cmd) == 0 {
		return "The command"
	}
	name := filepath.Base(strings.ReplaceAll(cmd[0], "\\", "/"))
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".exe") {
		name = strings.TrimSuffix(name, ext)
	}
	return name
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

	// Re-check under the write lock: concurrent fallbacks (thread dump, GC, heap)
	// may race here, and each extra copy would be left behind in the temp dir
	// because RemoveFromTempPath only removes the one stored in tmpPath.
	if len(tmpPath) > 0 {
		name = tmpPath
		return
	}

	path := Executable()
	if len(path) < 1 {
		err = errors.New("failed to Copy2TempPath: invalid path")
		return
	}
	in, err := os.Open(path)
	if err != nil {
		return
	}
	defer func() {
		_ = in.Close()
	}()
	// Windows only executes files with an executable extension, so the
	// temp copy must end in .exe there.
	pattern := "yc"
	if runtime.GOOS == "windows" {
		pattern = "yc*.exe"
	}
	file, err := os.CreateTemp("", pattern)
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
	tmpPathMtx.Lock()
	name := tmpPath
	// Forget the path so a later Copy2TempPath makes a fresh copy instead of
	// returning the file deleted here.
	tmpPath = ""
	tmpPathMtx.Unlock()

	if len(name) > 0 {
		os.Remove(name)
	}
}
