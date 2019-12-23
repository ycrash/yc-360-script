package capture

import (
	"fmt"
	"io"
	"os"

	"shell"
	"shell/logger"
)

const tdOut = "threaddump.out"

type ThreadDump struct {
	Capture
	Pid    int
	TdPath string
}

func (t *ThreadDump) Run() (result Result, err error) {
	var td *os.File
	if len(t.TdPath) > 0 {
		var tdf *os.File
		tdf, err = os.Open(t.TdPath)
		if err != nil {
			logger.Log("failed to open tdPath(%s), err: %s", t.TdPath, err.Error())
		} else {
			defer tdf.Close()
			td, err = os.Create(tdOut)
			if err != nil {
				return
			}
			defer td.Close()
			_, err = io.Copy(td, tdf)
			if err != nil {
				return
			}
			_, err = td.Seek(0, 0)
			if err != nil {
				return
			}

		}
	}
	if td == nil {
		// 1: concatenate individual thread dumps
		err = shell.CommandRun(shell.AppendJavaCoreFiles)
		if err != nil {
			return
		}
		// 2: Append top -H output file.
		err = shell.CommandRun(shell.AppendTopH, fmt.Sprintf("cat topdashH.%d.out >> ./threaddump.out", t.Pid))
		if err != nil {
			return
		}
		// 3: Transmit Thread dump
		td, err = os.Open(tdOut)
		if err != nil {
			return
		}
		defer td.Close()
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "td", td)
	return
}
