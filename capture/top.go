package capture

import (
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"strconv"

	"shell"
	"shell/logger"
)

type Top struct {
	Capture
}

func (t *Top) Run() (result Result, err error) {
	if len(shell.Top) < 1 {
		result.Msg = "skipped capturing TopH"
		result.Ok = false
		return
	}
	file, err := os.Create("top.out")
	if err != nil {
		return
	}
	defer func() {
		e := file.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
	}()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(file, shell.Top)
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Top"
		result.Ok = false
		return
	}
	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}
	if t.Cmd.ExitCode() != 0 && len(shell.Top2) > 0 {
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		output, rErr := ioutil.ReadAll(file)
		oCmd := t.Cmd
		err = file.Truncate(0)
		if err != nil {
			return
		}
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(file, shell.Top2)
		if err != nil {
			return
		}
		logger.Log("trying %q, cause %q exit code != 0, read err %s %v", t.Cmd.String(), oCmd, output, rErr)
		if t.Cmd.IsSkipped() {
			result.Msg = "skipped capturing Top"
			result.Ok = false
			return
		}
		err = t.Cmd.Wait()
		if err != nil {
			logger.Log("failed to wait cmd: %s", err.Error())
		}
	}
	e := file.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "top", file)
	return
}

type TopH struct {
	Capture
	Pid int
	N   int
}

func (t *TopH) Run() (result Result, err error) {
	if len(shell.TopH) < 1 {
		result.Msg = "skipped capturing TopH"
		result.Ok = false
		return
	}
	if !shell.IsProcessExists(t.Pid) {
		err = fmt.Errorf("process %d does not exist", t.Pid)
		return
	}
	logger.Log("Collection of top dash H data started for PID %d.", t.Pid)
	file, err := os.Create(fmt.Sprintf("topdashH.%d.out", t.N))
	if err != nil {
		return
	}
	defer func() {
		e := file.Sync()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to sync file %s", e)
		}
		e = file.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
	}()

	command, err := shell.TopH.AddDynamicArg(strconv.Itoa(t.Pid))
	if err != nil {
		return
	}
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(file, command)
	if err != nil && !errors.Is(err, exec.ErrNotFound) {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing TopH"
		result.Ok = false
		return
	}
	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}
	if t.Cmd.ExitCode() != 0 && len(shell.TopH2) > 0 {
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		output, rErr := ioutil.ReadAll(file)
		oCmd := t.Cmd
		err = file.Truncate(0)
		if err != nil {
			return
		}
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(file, shell.Append(shell.TopH2, strconv.Itoa(t.Pid)))
		if err != nil {
			return
		}
		logger.Log("trying %q, cause %q exit code != 0, read err %s %v", t.Cmd.String(), oCmd, output, rErr)
		if t.Cmd.IsSkipped() {
			result.Msg = "skipped capturing TopH"
			result.Ok = false
			return
		}
		err = t.Cmd.Wait()
		if err != nil {
			logger.Log("failed to wait cmd: %s", err.Error())
		}
	}
	return
}
