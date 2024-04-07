package capture

import (
	"io"
	"io/ioutil"
	"os"
	"runtime"
	"strconv"

	"shell/internal/capture/executils"
	"shell/internal/logger"
)

type VMStat struct {
	Capture
}

func (t *VMStat) Run() (result Result, err error) {
	file, err := os.Create("vmstat.out")
	if err != nil {
		return
	}
	defer file.Close()
	cmd, err := executils.VMState.AddDynamicArg(
		strconv.Itoa(executils.VMSTAT_INTERVAL),
		"5")
	if err != nil {
		return
	}

	t.Cmd, err = executils.CommandStartInBackgroundToWriter(file, cmd)
	if t.Cmd.IsSkipped() {
		result.Msg = "skipped capturing VMStat"
		result.Ok = false
		return
	}
	if err != nil {
		if runtime.GOOS != "linux" {
			return
		}
	}
	t.Cmd.Wait()
	file.Sync()

	if t.Cmd.ExitCode() != 0 && runtime.GOOS == "linux" {
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		output, rErr := ioutil.ReadAll(file)
		oCmd := t.Cmd
		err = file.Truncate(0)
		if err != nil {
			return
		}
		_, err = file.Seek(0, io.SeekStart)
		if err != nil {
			return
		}
		cmd, err = (&executils.Command{
			executils.WaitCommand,
			executils.Executable(),
			"-vmstatMode",
			executils.DynamicArg,
			executils.DynamicArg,
			`| awk '{cmd="(date +'%H:%M:%S')"; cmd | getline now; print now $0; fflush(); close(cmd)}'`,
		}).AddDynamicArg(
			strconv.Itoa(executils.VMSTAT_INTERVAL),
			"5")
		logger.Info().Strs("cmd", cmd).Err(rErr).Bytes("output", output).Str("failed cmd", oCmd.String()).Msg("vmstat failed, trying to use -vmstatMode")
		if err != nil {
			return
		}
		t.Cmd, err = executils.CommandStartInBackgroundToWriter(file, cmd)
		if err != nil {
			return
		}
		t.Cmd.Wait()
		file.Sync()
	}

	result.Msg, result.Ok = PostData(t.Endpoint(), "vmstat", file)
	return
}
