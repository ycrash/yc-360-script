package capture

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"

	"shell"
	"shell/logger"
)

const hdOut = "heap_dump.out"

type HeapDump struct {
	Capture
	JavaHome string
	Pid      int
	hdPath   string
}

func NewHeapDump(javaHome string, pid int, hdPath string) *HeapDump {
	return &HeapDump{JavaHome: javaHome, Pid: pid, hdPath: hdPath}
}

func (t *HeapDump) Run() (result Result, err error) {
	var hd *os.File
	if len(t.hdPath) > 0 {
		var hdf *os.File
		hdf, err = os.Open(t.hdPath)
		if err != nil {
			logger.Log("failed to open hdPath(%s) err: %s", t.hdPath, err.Error())
		} else {
			defer hdf.Close()
			hd, err = os.Create(hdOut)
			if err != nil {
				return
			}
			defer hd.Close()
			_, err = io.Copy(hd, hdf)
			if err != nil {
				return
			}
			_, err = hd.Seek(0, 0)
			if err != nil {
				return
			}
		}
	}
	if hd == nil {
		var dir string
		dir, err = os.Getwd()
		if err != nil {
			return
		}
		var output []byte
		output, err = shell.CommandCombinedOutput(shell.Command{path.Join(t.JavaHome, "/bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_dump", filepath.Join(dir, hdOut)})
		if err != nil {
			if len(output) > 1 {
				err = fmt.Errorf("%w because %s", err, output)
			}
			return
		}
		hd, err = os.Open(hdOut)
		if err != nil {
			err = fmt.Errorf("failed to open heap dump file: %w", err)
			return
		}
		defer hd.Close()
	}
	zipfile, err := os.Create("heap_dump.zip")
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	defer zipfile.Close()
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
