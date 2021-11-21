package capture

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"shell"
	"shell/logger"
)

type Top struct {
	Capture
}

func (t *Top) Run() (result Result, err error) {
	top, err := os.Create("top.out")
	if err != nil {
		return
	}
	defer func() {
		e := top.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
	}()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Top"
		result.Ok = true
		return
	}
	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}
	e := top.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "top", top)
	return
}

type TopH struct {
	Capture
	Pid int
	N   int
}

func (t *TopH) Run() (result Result, err error) {
	if !shell.IsProcessExists(t.Pid) {
		err = fmt.Errorf("process %d does not exist", t.Pid)
		return
	}
	logger.Log("Collection of top dash H data started for PID %d.", t.Pid)
	topdash, err := os.Create(fmt.Sprintf("topdashH.%d.out", t.N))
	if err != nil {
		return
	}
	defer func() {
		e := topdash.Sync()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to sync file %s", e)
		}
		e = topdash.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
	}()

	command, err := shell.TopH.AddDynamicArg(strconv.Itoa(t.Pid))
	if err != nil {
		return
	}
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(topdash, command)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing TopH"
		result.Ok = true
		return
	}
	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}
	return
}

type Top4M3 struct {
	Capture
}

func (t *Top4M3) Run() (result Result, err error) {
	top, err := os.Create("top4m3.out")
	if err != nil {
		return
	}
	defer func() {
		e := top.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
	}()

	for i := 0; i < 3; i++ {
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top4M3)
		if err != nil {
			return
		}
		if t.Cmd.IsSkipped() {
			result.Msg = "skipped capturing Top"
			result.Ok = true
			return
		}
		err = t.Cmd.Wait()
		if err != nil {
			logger.Log("failed to wait cmd: %s", err.Error())
		}
		_, err = top.WriteString("\n\n\n")
		if err != nil {
			logger.Log("failed to insert line break: %s", err.Error())
		}
		if i == 2 {
			break
		}
		time.Sleep(20 * time.Second)
	}
	e := top.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "top", top)
	return
}
