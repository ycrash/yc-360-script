package capture

import (
	"fmt"
	"os"
	"strconv"

	"shell"
)

type Top struct {
	Capture
}

func (t *Top) Run() (result Result, err error) {
	top, err := os.Create("top.out")
	if err != nil {
		return
	}
	defer top.Close()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top,
		"-d", strconv.Itoa(shell.TOP_INTERVAL),
		"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_INTERVAL+1))
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Top"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "top", top)
	return
}

type TopH struct {
	Capture
	Pid int
}

func (t *TopH) Run() (result Result, err error) {
	topdash, err := os.Create(fmt.Sprintf("topdashH.%d.out", t.Pid))
	if err != nil {
		return
	}
	defer topdash.Close()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(topdash, shell.TopH,
		"-d", strconv.Itoa(shell.TOP_DASH_H_INTERVAL),
		"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_DASH_H_INTERVAL+1),
		"-p", strconv.Itoa(t.Pid))
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing TopH"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	return
}
