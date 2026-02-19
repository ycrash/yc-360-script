package executils

import (
	"io"
	"os"
	"os/exec"
	"time"
)

type CmdManager interface {
	KillAndWait() (err error)
	GracefulStop(grace time.Duration) error
	IsSkipped() bool
	Wait() (err error)
	Interrupt() (err error)
	Kill() (err error)
	CombinedOutput() ([]byte, error)
	Run() error
	Start() error
	SetStdoutAndStderr(io.Writer)
	GetPid() int
	ExitCode() (code int)
	String() string
}

type WaitCmd struct {
	*exec.Cmd
}

func (c *WaitCmd) SetStdoutAndStderr(writer io.Writer) {
	if c.Cmd == nil {
		return
	}
	c.Stdout = writer
	c.Stderr = writer
}

func (c *WaitCmd) GetPid() int {
	if c.Cmd == nil || c.Process == nil {
		return -1
	}
	return c.Process.Pid
}

func (c *WaitCmd) KillAndWait() (err error) {
	return
}

func (c *WaitCmd) GracefulStop(_ time.Duration) error {
	return nil
}

func (c *WaitCmd) IsSkipped() bool {
	return c.Cmd == nil
}

func (c *WaitCmd) Wait() (err error) {
	if c.Cmd == nil {
		return
	}
	err = c.Cmd.Wait()
	return
}

func (c *WaitCmd) ExitCode() (code int) {
	if c.Cmd == nil {
		code = -1
		return
	}
	code = c.ProcessState.ExitCode()
	return
}

func (c *WaitCmd) Interrupt() (err error) {
	return
}

func (c *WaitCmd) Kill() (err error) {
	return
}

func (c *WaitCmd) String() string {
	if c.Cmd == nil {
		return ""
	}
	return c.Cmd.String()
}

type Cmd struct {
	WaitCmd
}

func (c *Cmd) KillAndWait() (err error) {
	if c.Cmd == nil || c.Process == nil {
		return
	}
	err = c.Process.Kill()
	if err != nil {
		return
	}
	// Reap the process to release OS resources (file descriptors, exit status).
	// The result is ignored because exit code is meaningless after a hard kill.
	_ = c.Cmd.Wait()
	return
}

func (c *Cmd) GracefulStop(grace time.Duration) error {
	if c.Cmd == nil || c.Process == nil {
		return nil
	}

	if err := c.Process.Signal(os.Interrupt); err != nil {
		return c.KillAndWait()
	}

	done := make(chan error, 1)
	go func() { done <- c.Cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-time.After(grace):
		_ = c.Process.Kill()
		return <-done
	}
}

func (c *Cmd) Interrupt() (err error) {
	if c.Cmd == nil || c.Process == nil {
		return
	}
	err = c.Process.Signal(os.Interrupt)
	return
}

func (c *Cmd) Kill() (err error) {
	if c.Cmd == nil || c.Process == nil {
		return
	}
	err = c.Process.Kill()
	return
}
