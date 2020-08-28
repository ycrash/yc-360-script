package capture

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"shell"
	"shell/logger"
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
	switch runtime.GOOS {
	case "linux":
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top,
			"-d", strconv.Itoa(shell.TOP_INTERVAL),
			"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_INTERVAL+1))
	case "aix":
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top)
	}
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
	Pid       int
	WaitGroup sync.WaitGroup
}

func (t *TopH) Run() (result Result, err error) {
	defer t.WaitGroup.Done()

	if !shell.IsProcessExists(t.Pid) {
		err = fmt.Errorf("process %d does not exist", t.Pid)
		return
	}
	logger.Log("Collection of top dash H data started for PID %d.", t.Pid)
	topdash, err := os.Create(fmt.Sprintf("topdashH.%d.out", t.Pid))
	if err != nil {
		return
	}
	defer topdash.Close()

	switch runtime.GOOS {
	case "linux":
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(topdash, shell.TopH,
			"-d", strconv.Itoa(shell.TOP_DASH_H_INTERVAL),
			"-n", strconv.Itoa(shell.SCRIPT_SPAN/shell.TOP_DASH_H_INTERVAL+1),
			"-p", strconv.Itoa(t.Pid))
	case "aix":
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(topdash, shell.TopH)
	}

	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing TopH"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "toph", topdash)
	return
}

type Top4AP struct {
	Capture
}

func (t *Top4AP) Run() (result Result, err error) {
	top, err := os.Create("top.out")
	if err != nil {
		return
	}
	defer top.Close()

	for i := 0; i < 3; i++ {
		t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top)
		if err != nil {
			return
		}
		if t.Cmd.IsSkipped() {
			result.Msg = "skipped capturing Top"
			result.Ok = true
			return
		}
		time.Sleep(time.Second)
		t.Kill()
		t.Cmd.Wait()
		top.WriteString("\n\n\n")
		if i == 2 {
			break
		}
		time.Sleep(10 * time.Second)
	}
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "top", top)
	return
}
