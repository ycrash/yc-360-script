package capture

import (
	"os"

	"shell/internal/utils"
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
	k.Cmd, err = utils.CommandStartInBackgroundToWriter(kernel, utils.KernelParam)
	if err != nil {
		return
	}
	if k.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Kernel"
		result.Ok = false
		return
	}
	k.Cmd.Wait()
	result.Msg, result.Ok = utils.PostData(k.Endpoint(), "kernel", kernel)
	return
}
