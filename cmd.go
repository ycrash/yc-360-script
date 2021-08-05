package shell

import (
	"io"
	"os"
	"os/exec"
)

type CmdManager interface {
	KillAndWait() (err error)
	IsSkipped() bool
	Wait()
	Interrupt() (err error)
	Kill() (err error)
	CombinedOutput() ([]byte, error)
	Run() error
	Start() error
	SetStdoutAndStderr(io.Writer)
	GetPid() int
	ExitCode() (code int)
}

type WaitCmd struct {
	*exec.Cmd
}

func (c *WaitCmd) SetStdoutAndStderr(writer io.Writer) {
	if c.Cmd == nil {
		return
	}
	c.Cmd.Stdout = writer
	c.Cmd.Stderr = writer
}

func (c *WaitCmd) GetPid() int {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return -1
	}
	return c.Cmd.Process.Pid
}

func (c *WaitCmd) KillAndWait() (err error) {
	return
}

func (c *WaitCmd) IsSkipped() bool {
	if c.Cmd == nil {
		return true
	}
	return false
}

func (c *WaitCmd) Wait() {
	if c.Cmd == nil {
		return
	}
	_ = c.Cmd.Wait()
}

func (c *WaitCmd) ExitCode() (code int) {
	if c.Cmd == nil {
		code = -1
		return
	}
	code = c.Cmd.ProcessState.ExitCode()
	return
}

func (c *WaitCmd) Interrupt() (err error) {
	return
}

func (c *WaitCmd) Kill() (err error) {
	return
}

type Cmd struct {
	WaitCmd
}

func (c *Cmd) KillAndWait() (err error) {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return
	}
	err = c.Cmd.Process.Kill()
	if err != nil {
		return
	}
	_ = c.Cmd.Wait()
	return
}

func (c *Cmd) Interrupt() (err error) {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return
	}
	err = c.Cmd.Process.Signal(os.Interrupt)
	return
}

func (c *Cmd) Kill() (err error) {
	if c.Cmd == nil || c.Cmd.Process == nil {
		return
	}
	err = c.Cmd.Process.Kill()
	return
}
