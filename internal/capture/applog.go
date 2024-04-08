package capture

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"yc-agent/internal/config"

	"github.com/mattn/go-zglob"
)

var compressedFileExtensions = []string{
	"zip",
	"gz",
}

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
	fileExt := filepath.Ext(filePath)          // .zip, .log
	fileExt = strings.TrimPrefix(fileExt, ".") // zip, log
	isCompressed := isCompressedFileExt(fileExt)

	// Initialize a counter variable
	counter := 1

	// Generate a unique filename by appending the sequential number
	dstFileName := fmt.Sprintf("%d.appLogs.%s", counter, fileBaseName) // Example: 1.appLogs.abc.log

	// Check if the file already exists with the generated name
	for fileExists(dstFileName) {
		// If the file exists, increment the counter and generate a new filename
		counter++
		dstFileName = fmt.Sprintf("%d.appLogs.%s", counter, fileBaseName) // Example: 2.appLogs.abc.log
	}

	dst, err := os.Create(dstFileName)

	if err != nil {
		return
	}
	defer dst.Close()

	if t.N == 0 {
		t.N = 3000
	}

	if !isCompressed {
		err = PositionLastLines(src, t.N)
		if err != nil {
			return
		}
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

	dt := "applog&logName=" + fileBaseName
	if isCompressed {
		dt = dt + "&content-encoding=" + fileExt
	}

	result.Msg, result.Ok = PostData(t.Endpoint(), dt, dst)

	return
}

func isCompressedFileExt(s string) bool {
	for _, ext := range compressedFileExtensions {
		if ext == s {
			return true
		}
	}

	return false
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
