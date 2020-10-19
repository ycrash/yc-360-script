package capture

import (
	"fmt"
	"os"
	"strconv"
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
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(top, shell.Top)
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
	N   int
}

func (t *TopH) Run() (result Result, err error) {
	if !shell.IsProcessExists(t.Pid) {
		err = fmt.Errorf("process %d does not exist", t.Pid)
		return
	}
	logger.Log("Collection of top dash H data started for PID %d.", t.Pid)
	topdash, err := os.Create(fmt.Sprintf("topdashH.%d.out", t.N))
	if err != nil {
		return
	}
	defer topdash.Close()

	command, err := shell.TopH.AddDynamicArg(strconv.Itoa(t.Pid))
	if err != nil {
		return
	}
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(topdash, command)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing TopH"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	return
}

type Top4M3 struct {
	Capture
}

func (t *Top4M3) Run() (result Result, err error) {
	top, err := os.Create("top4m3.out")
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
