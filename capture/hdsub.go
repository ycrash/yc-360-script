package capture

import (
	"errors"
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
		e := out.Sync()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to sync file %s", e)
		}
		e = out.Close()
		if e != nil && !errors.Is(e, os.ErrClosed) {
			logger.Log("failed to close file %s", e)
		}
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
			logger.Log("Failed to capture GC.class_histogram with err %v.", err)
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
			logger.Log("Failed to capture VM.system_properties with err %v.", err)
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
			logger.Log("Failed to capture GC.heap_info with err %v.", err)
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
			logger.Log("Failed to capture VM.flags with err %v.", err)
		}
	}
	e := out.Sync()
	if e != nil && !errors.Is(e, os.ErrClosed) {
		logger.Log("failed to sync file %s", e)
	}

	result.Msg, result.Ok = shell.PostData(t.Endpoint(), "hdsub", out)
	return
}
