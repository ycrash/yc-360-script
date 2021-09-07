package capture

import (
	"archive/zip"
	"bufio"
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
			logger.Log("try to read in docker, because failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
			hdf, err = os.Open(filepath.Join("/proc", strconv.Itoa(t.Pid), "root", t.hdPath))
		}
		if err != nil {
			logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
		} else {
			logger.Log("copying heap dump data %s", t.hdPath)
			defer func() {
				err := hdf.Close()
				if err != nil {
					logger.Log("failed to close hd file %s", t.hdPath)
				}
			}()
			hd, err = os.Create(hdOut)
			if err != nil {
				return
			}
			defer func() {
				err := hd.Close()
				if err != nil {
					logger.Log("failed to close hd file %s", hdOut)
				}
				err = os.Remove(hdOut)
				if err != nil {
					logger.Log("failed to rm hd file %s", hdOut)
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
		if err != nil {
			if len(output) > 1 {
				err = fmt.Errorf("%w because %s", err, output)
			}
			var e2 error
			fp = filepath.Join(os.TempDir(), fmt.Sprintf("%s.%d", hdOut, t.Pid))
			output, e2 = shell.CommandCombinedOutput(shell.Command{shell.JAttach, strconv.Itoa(t.Pid), "dumpheap", fp})
			if e2 != nil {
				err = fmt.Errorf("%w, %v", e2, err)
				return
			}
		}
		hd, err = os.Open(fp)
		if err != nil && runtime.GOOS == "linux" {
			logger.Log("try to open file in docker, because failed to %v", err)
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
				logger.Log("failed to close hd file %s", fp)
			}
			err = os.Remove(fp)
			if err != nil {
				logger.Log("failed to rm hd file %s", fp)
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
	defer func() {
		err := zipfile.Close()
		if err != nil {
			logger.Log("failed to close hd zip file %s", hdZip)
		}
	}()
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
