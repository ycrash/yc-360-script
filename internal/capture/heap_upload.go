package capture

import (
	"fmt"
	"io"
	"os"
	"time"
	"yc-agent/internal/config"
)

func uploadCapturedFileWithZstdCompression(endpoint, dt string, file *os.File) Result {
	if config.GlobalConfig.OnlyCapture {
		return Result{Msg: "in only capture mode"}
	}
	if file == nil {
		return Result{Msg: "file is not captured"}
	}
	stat, err := file.Stat()
	if err != nil {
		return Result{Msg: fmt.Sprintf("file stat err %s", err.Error())}
	}
	if stat.Size() < 1 {
		fileName := stat.Name()
		return Result{Msg: fmt.Sprintf("skipped empty file %s", fileName)}
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{
			Msg: fmt.Sprintf("failed seeking to beginning of heap dump file: %s", err.Error()),
			Ok:  false,
		}
	}

	pr, pw := io.Pipe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		enc, err := newZstdEncoder(pw)
		if err != nil {
			pw.CloseWithError(err)
			return
		}

		_, copyErr := io.Copy(enc, file)
		closeErr := enc.Close()
		if copyErr == nil {
			copyErr = closeErr
		}

		pw.CloseWithError(copyErr)
	}()

	msg, ok := PostReaderWithTimeout(endpoint, dt+"&Content-Encoding=zst", pr, 0*time.Second)

	pr.CloseWithError(io.ErrClosedPipe)
	<-done

	return Result{
		Msg: msg,
		Ok:  ok,
	}
}
