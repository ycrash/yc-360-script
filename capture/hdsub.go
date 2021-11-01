package capture

import (
	"os"
	"path"
	"shell"
	"shell/logger"
	"strconv"
)

type HDSub struct {
	Capture
	JavaHome string
	Pid      int
}

func (t *HDSub) Run() (result Result, err error) {
	fn := "hdsub.out"
	out, err := os.Create(fn)
	if err != nil {
		return
	}
	defer func() {
		_ = out.Close()
	}()
	_, err = out.WriteString("GC.class_histogram:\n")
	if err != nil {
		logger.Log("failed to write file %s", err)
	}
	err = shell.CommandCombinedOutputToWriter(out,
		shell.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), "GC.class_histogram"})
	if err != nil {
		logger.Log("Failed to run jcmd with err %v. Trying to capture using jattach...", err)
		err = shell.CommandCombinedOutputToWriter(out,
			shell.Command{shell.Executable(t.Pid), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", "GC.class_histogram"})
		if err != nil {
			return
		}
	}
	_, err = out.WriteString("\nVM.system_properties:\n")
	if err != nil {
		logger.Log("failed to write file %s", err)
	}
	err = shell.CommandCombinedOutputToWriter(out,
		shell.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), "VM.system_properties"})
	if err != nil {
		logger.Log("Failed to run jcmd with err %v. Trying to capture using jattach...", err)
		err = shell.CommandCombinedOutputToWriter(out,
			shell.Command{shell.Executable(t.Pid), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", "VM.system_properties"})
		if err != nil {
			return
		}
	}
	_, err = out.WriteString("\nGC.heap_info:\n")
	if err != nil {
		logger.Log("failed to write file %s", err)
	}
	err = shell.CommandCombinedOutputToWriter(out,
		shell.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), "GC.heap_info"})
	if err != nil {
		logger.Log("Failed to run jcmd with err %v. Trying to capture using jattach...", err)
		err = shell.CommandCombinedOutputToWriter(out,
			shell.Command{shell.Executable(t.Pid), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", "GC.heap_info"})
		if err != nil {
			return
		}
	}
	_, err = out.WriteString("\nVM.flags:\n")
	if err != nil {
		logger.Log("failed to write file %s", err)
	}
	err = shell.CommandCombinedOutputToWriter(out,
		shell.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), "VM.flags"})
	if err != nil {
		logger.Log("Failed to run jcmd with err %v. Trying to capture using jattach...", err)
		err = shell.CommandCombinedOutputToWriter(out,
			shell.Command{shell.Executable(t.Pid), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", "VM.flags"})
		if err != nil {
			return
		}
	}
	err = out.Sync()
	if err != nil {
		logger.Log("failed to sync, err: %s", err.Error())
	}

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "hdsub", out)
	return
}
