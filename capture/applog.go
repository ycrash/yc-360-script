package capture

import (
	"io"
	"os"
	"shell/logger"

	"shell"
)

const appOut = "applog.out"

type AppLog struct {
	Capture
	Path string
	N    uint
}

func (t *AppLog) Run() (result Result, err error) {
	if len(t.Path) <= 0 {
		return
	}
	var dst *os.File
	var src *os.File
	src, err = os.Open(t.Path)
	if err != nil {
		logger.Log("failed to open applog(%s), err: %s", t.Path, err.Error())
		return
	}
	defer func() {
		err := src.Close()
		if err != nil {
			logger.Log("failed to close, err: %s", err.Error())
		}
	}()
	dst, err = os.Create(appOut)
	if err != nil {
		return
	}
	defer func() {
		_ = dst.Close()
	}()
	if t.N == 0 {
		t.N = 1000
	}
	err = shell.PositionLastLines(src, t.N)
	if err != nil {
		return
	}
	_, err = io.Copy(dst, src)
	if err != nil {
		return
	}
	err = dst.Sync()
	if err != nil {
		logger.Log("failed to sync, err: %s", err.Error())
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "applog", dst)
	return
}
