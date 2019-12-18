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
)

type HeapDump struct {
	Capture
	JavaHome string
	Pid      int
}

func NewHeapDump(javaHome string, pid int) *HeapDump {
	return &HeapDump{JavaHome: javaHome, Pid: pid}
}

func (t *HeapDump) Run() (result Result, err error) {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	output, err := shell.CommandCombinedOutput(shell.Command{path.Join(t.JavaHome, "/bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_dump", filepath.Join(dir, "/heap_dump.out")})
	if err != nil {
		if len(output) > 1 {
			err = fmt.Errorf("%w because %s", err, output)
		}
		return
	}
	zipfile, err := os.Create("heap_dump.zip")
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	defer zipfile.Close()
	writer := zip.NewWriter(bufio.NewWriter(zipfile))
	out, err := writer.Create("heap_dump.out")
	if err != nil {
		err = fmt.Errorf("failed to create zip file: %w", err)
		return
	}
	hdout, err := os.Open("heap_dump.out")
	if err != nil {
		err = fmt.Errorf("failed to open heap dump file: %w", err)
		return
	}
	defer hdout.Close()
	_, err = io.Copy(out, hdout)
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
