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
	cmd, err := shell.VMState.AddDynamicArg(
		strconv.Itoa(shell.VMSTAT_INTERVAL),
		strconv.Itoa(shell.SCRIPT_SPAN/shell.VMSTAT_INTERVAL+1))
	if err != nil {
		return
	}

	t.Cmd, err = shell.CommandStartInBackgroundToWriter(vmstat, cmd)
	if err != nil {
		return
	}
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing VMStat"
		result.Ok = true
		return
	}
	t.Cmd.Wait()
	vmstat.Sync()

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "vmstat", vmstat)
	return
}
