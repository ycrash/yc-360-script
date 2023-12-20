package capture

import (
	"errors"
	"os"
	"time"

	"shell"
	"shell/logger"
)

type Top4M3 struct {
	Capture
}

func (t *Top4M3) Run() (result Result, err error) {
	if len(shell.Top4M3) < 1 {
		result.Msg = "skipped capturing TopH"
		result.Ok = false
		return
	}
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
			result.Ok = false
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
