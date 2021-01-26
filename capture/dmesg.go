package capture

import (
	"os"

	"shell"
)

type DMesg struct {
	Capture
}

func (t *DMesg) Run() (result Result, err error) {
	file, err := os.Create("dmesg.out")
	if err != nil {
		return
	}
	defer file.Close()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(file, shell.DMesg)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing DMesg"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "dmesg", file)
	return
}
