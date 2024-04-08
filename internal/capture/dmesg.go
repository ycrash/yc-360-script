package capture

import (
	"errors"
	"io"
	"io/ioutil"
	"os"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

type DMesg struct {
	Capture
}

func (t *DMesg) Run() (result Result, err error) {
	file, err := os.Create("dmesg.out")
	if err != nil {
		return
	}
	defer file.Close()
	t.Cmd, err = executils.CommandStartInBackgroundToWriter(file, executils.DMesg)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing DMesg"
		result.Ok = true
		return
	}
	err = t.Cmd.Wait()
	if err != nil {
		logger.Log("failed to wait cmd: %s", err.Error())
	}
	if t.Cmd.ExitCode() != 0 {
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
		t.Cmd, err = executils.CommandStartInBackgroundToWriter(file, executils.DMesg2)
		if err != nil {
			return
		}
		logger.Log("trying %s, cause %s exit code != 0, read err %s %v", t.Cmd.String(), oCmd, output, rErr)
		if t.Cmd.IsSkipped() {
			result.Msg = "skipped capturing DMesg"
			result.Ok = true
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
	result.Msg, result.Ok = PostData(t.Endpoint(), "dmesg", file)
	return
}
