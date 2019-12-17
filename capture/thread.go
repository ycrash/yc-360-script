package capture

import (
	"fmt"
	"os"

	"shell"
)

type ThreadDump struct {
	Capture
	Pid int
}

func (t *ThreadDump) Run() (result Result, err error) {
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
	td, err := os.Open("threaddump.out")
	if err != nil {
		return
	}
	defer td.Close()
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "td", td)
	return
}
