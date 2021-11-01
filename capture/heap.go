package capture

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"

	"shell"
	"shell/logger"
)

const hdOut = "heap_dump.out"
const hdZip = "heap_dump.zip"

type HeapDump struct {
	Capture
	JavaHome string
	Pid      int
	hdPath   string
	dump     bool
}

func NewHeapDump(javaHome string, pid int, hdPath string, dump bool) *HeapDump {
	return &HeapDump{JavaHome: javaHome, Pid: pid, hdPath: hdPath, dump: dump}
}

func (t *HeapDump) Run() (result Result, err error) {
	var hd *os.File
	if len(t.hdPath) > 0 {
		var hdf *os.File
		hdf, err = os.Open(t.hdPath)
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("failed to open hdPath(%s) err: %s. Trying to open in the Docker container...", t.hdPath, err.Error())
			hdf, err = os.Open(filepath.Join("/proc", strconv.Itoa(t.Pid), "root", t.hdPath))
		}
		if err != nil {
			logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
		} else {
			logger.Log("copying heap dump data %s", t.hdPath)
			defer func() {
				err := hdf.Close()
				if err != nil {
					logger.Log("failed to close hd file %s cause err: %s", t.hdPath, err.Error())
				}
			}()
			hd, err = os.Create(hdOut)
			if err != nil {
				return
			}
			defer func() {
				err := hd.Close()
				if err != nil {
					logger.Log("failed to close hd file %s cause err: %s", hdOut, err.Error())
				}
				err = os.Remove(hdOut)
				if err != nil {
					logger.Log("failed to rm hd file %s cause err: %s", hdOut, err.Error())
				}
			}()
			_, err = io.Copy(hd, hdf)
			if err != nil {
				return
			}
			_, err = hd.Seek(0, 0)
			if err != nil {
				return
			}
			logger.Log("copied heap dump data %s", t.hdPath)
		}
	}
	if t.Pid > 0 && hd == nil && t.dump {
		logger.Log("capturing heap dump data")
		var dir string
		dir, err = os.Getwd()
		if err != nil {
			return
		}
		var output []byte
		fp := filepath.Join(dir, hdOut)
		output, err = shell.CommandCombinedOutput(shell.Command{path.Join(t.JavaHome, "/bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_dump", fp})
		logger.Log("Output from jcmd: %s, %v", output, err)
		if err != nil ||
			bytes.Index(output, []byte("No such file")) >= 0 ||
			bytes.Index(output, []byte("Permission denied")) >= 0 {
			if len(output) > 1 {
				err = fmt.Errorf("%w because %s", err, output)
			}
			var e2 error
			fp = filepath.Join(os.TempDir(), fmt.Sprintf("%s.%d", hdOut, t.Pid))
			output, e2 = shell.CommandCombinedOutput(shell.Command{shell.Executable(t.Pid), "-p", strconv.Itoa(t.Pid), "-hdPath", fp, "-hdCaptureMode"})
			logger.Log("Output from jattach: %s, %v", output, e2)
			if e2 != nil {
				err = fmt.Errorf("%v: %v", e2, err)
				return
			}
		}
		hd, err = os.Open(fp)
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("Failed to %s. Trying to open in the Docker container...", err.Error())
			fp = filepath.Join("/proc", strconv.Itoa(t.Pid), "root", fp)
			hd, err = os.Open(fp)
		}
		if err != nil {
			err = fmt.Errorf("failed to open heap dump file: %w", err)
			return
		}
		defer func() {
			err := hd.Close()
			if err != nil {
				logger.Log("failed to close hd file %s cause err: %s", fp, err.Error())
			}
			err = os.Remove(fp)
			if err != nil {
				logger.Log("failed to rm hd file %s cause err: %s", fp, err.Error())
			}
		}()
		logger.Log("captured heap dump data")
	}
	if hd == nil {
		if errors.Is(err, os.ErrNotExist) {
			err = nil
		}
		result.Msg = "skipped heap dump"
		return
	}
	zipfile, err := os.Create(hdZip)
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	writer := zip.NewWriter(bufio.NewWriter(zipfile))
	out, err := writer.Create(hdOut)
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	_, err = io.Copy(out, hd)
	if err != nil {
		err = fmt.Errorf("failed to zip heap dump file: %w", err)
		return
	}
	err = writer.Close()
	if err != nil {
		err = fmt.Errorf("failed to finish zipping heap dump file: %w", err)
		return
	}

	result.Msg, result.Ok = shell.PostData(t.endpoint, "hd&Content-Encoding=zip", zipfile)
	return
}
