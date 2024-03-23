package capture

import (
	"io"
	"os"

	"shell/internal/logger"
	"shell/internal/utils"
)

const accessLogOut = "accesslog.out"

// AccessLog is deprecated in favor of App logs auto discovery
type AccessLog struct {
	Capture
	Path     string
	Position int64
}

func (t *AccessLog) Run() (result Result, err error) {
	if len(t.Path) <= 0 {
		return
	}

	var dst *os.File
	var newPosition int64
	dst, newPosition, err = prepareLogsForSending(t.Path, t.Position, accessLogOut)
	if dst != nil {
		defer func() {
			_ = dst.Close()
		}()
	}

	if err != nil {
		result.Msg = err.Error()
		return
	}

	result.Msg, result.Ok = utils.PostData(t.Endpoint(), "accessLog", dst)
	t.Position = newPosition
	return
}

func prepareLogsForSending(srcFilePath string, position int64, dstFilePath string) (*os.File, int64, error) {
	var dst *os.File
	var src *os.File
	src, err := os.Open(srcFilePath)
	if err != nil {
		logger.Log("failed to open accesslog(%s), err: %s", srcFilePath, err.Error())
		return nil, position, err
	}
	defer func() {
		err := src.Close()
		if err != nil {
			logger.Log("failed to close, err: %s", err.Error())
		}
	}()
	dst, err = os.Create(dstFilePath)
	if err != nil {
		return dst, position, err
	}

	_, err = src.Seek(position, io.SeekStart)
	if err != nil {
		logger.Log("failed to seek in source file, err: %s", err.Error())
		return dst, position, err
	}
	copied, err := io.Copy(dst, src)
	if err != nil {
		logger.Log("unable to copy accessLog, err: %s", err.Error())
		return dst, position, err
	}

	err = dst.Sync()
	if err != nil {
		logger.Log("failed to sync, err: %s", err.Error())
	}

	return dst, position + copied, nil
}
