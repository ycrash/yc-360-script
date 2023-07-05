package capture

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"shell"
	"shell/config"

	"github.com/mattn/go-zglob"
)

type AppLog struct {
	Capture
	Paths config.AppLogs
	N     uint
}

func (t *AppLog) Run() (result Result, err error) {
	results := []Result{}
	errs := []error{}

	for _, path := range t.Paths {
		matches, err := zglob.Glob(string(path))

		if err != nil {
			r := Result{Msg: "invalid glob pattern: " + string(path), Ok: false}
			e := err

			results = append(results, r)
			errs = append(errs, e)
		} else {
			for _, match := range matches {
				r, e := t.CaptureSingleAppLog(match)

				results = append(results, r)
				errs = append(errs, e)
			}
		}
	}

	result, err = summarizeResults(results, errs)

	return
}

func (t *AppLog) CaptureSingleAppLog(filePath string) (result Result, err error) {
	src, err := os.Open(filePath)
	if err != nil {
		err = fmt.Errorf("failed to open applog(%s), err: %w", filePath, err)
		return
	}
	defer src.Close()

	fileBaseName := filepath.Base(filePath)

	// Initialize a counter variable
	counter := 1

	// Generate a unique filename by appending the sequential number
	dstFileName := fmt.Sprintf("applog.%d.%s", counter, fileBaseName) // Example: applog.1.abc.log

	// Check if the file already exists with the generated name
	for fileExists(dstFileName) {
		// If the file exists, increment the counter and generate a new filename
		counter++
		dstFileName = fmt.Sprintf("applog.%d.%s", counter, fileBaseName) // Example: applog.2.abc.log
	}

	dst, err := os.Create(dstFileName)

	if err != nil {
		return
	}
	defer dst.Close()

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
		err = fmt.Errorf("failed to sync: %w", err)
		return
	}

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "applog&logName="+fileBaseName, dst)

	return
}

func summarizeResults(results []Result, errs []error) (result Result, err error) {
	var buffer bytes.Buffer
	ok := false

	var lastErr error

	for i, r := range results {
		buffer.WriteString("Msg: ")
		buffer.WriteString(r.Msg)
		buffer.WriteString("\n")
		buffer.WriteString("Ok: ")

		if r.Ok {
			buffer.WriteString("true")
			ok = true
		} else {
			buffer.WriteString("false")
		}

		if errs[i] != nil {
			buffer.WriteString("\n")
			buffer.WriteString("Err: ")
			buffer.WriteString(errs[i].Error())
			lastErr = errs[i]
		}

		buffer.WriteString("\n----\n")
	}

	result.Msg = buffer.String()
	result.Ok = ok

	if !ok {
		err = lastErr
	}

	return
}
