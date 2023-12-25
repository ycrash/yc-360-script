package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"shell"
	"shell/config"
	"strings"

	"github.com/mattn/go-zglob"
)

// AppLogM3 capture files specified in Paths. The file contents are captured
// progressively each run. It maintains last read positions so that it only captures
// file content starting from previous position.
type AppLogM3 struct {
	Capture
	Paths config.AppLogs
	N     uint
}

func (t *AppLogM3) Run() (result Result, err error) {
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

func (t *AppLogM3) CaptureSingleAppLog(filePath string) (result Result, err error) {
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
		t.N = 1000
	}

	if !isCompressed {
		err = shell.PositionLastLines(src, t.N)
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

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), dt, dst)

	return
}
