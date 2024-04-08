package capture

import (
	"os"

	"yc-agent/internal/capture/executils"
)

type Kernel struct {
	Capture
}

func (k *Kernel) Run() (result Result, err error) {
	kernel, err := os.Create("kernel.out")
	if err != nil {
		return
	}
	defer kernel.Close()
	k.Cmd, err = executils.CommandStartInBackgroundToWriter(kernel, executils.KernelParam)
	if err != nil {
		return
	}
	if k.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Kernel"
		result.Ok = false
		return
	}
	k.Cmd.Wait()
	result.Msg, result.Ok = PostData(k.Endpoint(), "kernel", kernel)
	return
}
