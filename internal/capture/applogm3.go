package capture

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"shell"
	"shell/internal/config"
	"shell/internal/logger"
	"strconv"
	"strings"

	"github.com/mattn/go-zglob"
)

// AppLogM3 capture files specified in Paths. The file contents are captured
// progressively each run. It maintains last read positions so that it only captures
// file content starting from previous position.
type AppLogM3 struct {
	Capture
	Paths     map[int]config.AppLogs // int=pid
	readStats map[string]appLogM3ReadStat
}

type appLogM3ReadStat struct {
	filePath     string
	fileSize     int64
	readPosition int64
}

func NewAppLogM3() *AppLogM3 {
	return &AppLogM3{
		readStats: make(map[string]appLogM3ReadStat),
		Paths:     make(map[int]config.AppLogs),
	}
}

func (a *AppLogM3) SetPaths(p map[int]config.AppLogs) {
	a.Paths = p
}

func (a *AppLogM3) Run() (result Result, err error) {
	results := []Result{}
	errs := []error{}

	for pid, paths := range a.Paths {
		for _, path := range paths {
			matches, err := zglob.Glob(string(path))

			if err != nil {
				r := Result{Msg: "invalid glob pattern: " + string(path), Ok: false}
				e := err

				results = append(results, r)
				errs = append(errs, e)
			} else {
				for _, match := range matches {
					r, e := a.CaptureSingleAppLog(match, pid)

					results = append(results, r)
					errs = append(errs, e)
				}
			}
		}
	}

	result, err = summarizeResults(results, errs)

	return
}

func (a *AppLogM3) CaptureSingleAppLog(filePath string, pid int) (result Result, err error) {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		err = fmt.Errorf("failed to stat applog(%s), err: %w", filePath, err)
		return
	}

	src, err := os.Open(filePath)
	if err != nil {
		err = fmt.Errorf("failed to open applog(%s), err: %w", filePath, err)
		return
	}
	defer src.Close()

	readStat := a.readStats[filePath]
	readStat.filePath = filePath

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

	currentSize := fileInfo.Size()
	// If the file was truncated
	if currentSize > 0 && currentSize < readStat.fileSize {
		readStat.readPosition = 0
		logger.Log("applogm3: resetting read position because the file was truncated: %s", filePath)
	} else {
		_, errSeek := src.Seek(readStat.readPosition, io.SeekStart)
		logger.Log("applogm3: reading log file %s starting from pos: %d", filePath, readStat.readPosition)
		if errSeek != nil {
			readStat.readPosition = 0
		}
	}

	numWritten, err := io.Copy(dst, src)
	if err != nil {
		return
	}
	readStat.readPosition += numWritten

	err = dst.Sync()
	if err != nil {
		err = fmt.Errorf("failed to sync: %w", err)
		return
	}

	dt := "applog&logName=" + fileBaseName + "&pid=" + strconv.Itoa(pid)
	if isCompressed {
		dt = dt + "&content-encoding=" + fileExt
	}

	result.Msg, result.Ok = shell.PostData(a.Endpoint(), dt, dst)

	// Update readStats for next run
	readStat.fileSize = fileInfo.Size()
	a.readStats[filePath] = readStat

	return
}
