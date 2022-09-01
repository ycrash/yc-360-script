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
	Pid      int
	TdPath   string
	JavaHome string
}

func (t *ThreadDump) Run() (result Result, err error) {
	var td *os.File
	// Thread dump: Attempt 3: tdPath argument (real step is 1 )
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
		return
	}
	if t.Pid > 0 && td == nil {
		if !shell.IsProcessExists(t.Pid) {
			err = fmt.Errorf("process %d does not exist", t.Pid)
			return
		}

		logger.Log("Collecting thread dump...")
		capJStack := NewJStack(t.JavaHome, t.Pid)
		_, err = capJStack.Run()
		if err != nil {
			logger.Log("jstack error %s", err.Error())
		} else {
			logger.Log("Collected thread dump...")
		}
		err = shell.CommandRun(shell.AppendJavaCoreFiles)
		if err != nil {
			return
		}
		err = shell.CommandRun(shell.AppendTopHFiles)
		if err != nil {
			return
		}

		td, err = os.Open(tdOut)
		if err != nil {
			return
		}
		defer td.Close()
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "td", td)
	return
}
