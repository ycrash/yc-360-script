package capture

import (
	"os"
	"shell"
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
	k.Cmd, err = shell.CommandStartInBackgroundToWriter(kernel, shell.KernelParam)
	if err != nil {
		return
	}
	if k.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Kernel"
		result.Ok = false
		return
	}
	k.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(k.Endpoint(), "kernel", kernel)
	return
}
