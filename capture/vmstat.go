package capture

import (
	"os"
	"strconv"

	"shell"
)

type VMStat struct {
	Capture
}

func (t *VMStat) Run() (result Result, err error) {
	vmstat, err := os.Create("vmstat.out")
	if err != nil {
		return
	}
	defer vmstat.Close()
	t.Cmd, err = shell.CommandStartInBackgroundToWriter(vmstat, shell.VMState,
		strconv.Itoa(shell.VMSTAT_INTERVAL),
		strconv.Itoa(shell.SCRIPT_SPAN/shell.VMSTAT_INTERVAL+1))
	if err != nil {
		return
	}
	t.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "vmstat", vmstat)
	return
}
