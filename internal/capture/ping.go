package capture

import (
	"os"

	"yc-agent/internal/capture/executils"
)

type Ping struct {
	Capture
	Host string
}

func (c *Ping) Run() (result Result, err error) {
	file, err := os.Create("ping.out")
	if err != nil {
		return
	}
	defer file.Close()
	c.Cmd, err = executils.CommandStartInBackgroundToWriter(file, executils.Append(executils.Ping, c.Host))
	if err != nil {
		return
	}
	if c.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Ping"
		result.Ok = false
		return
	}
	c.Cmd.Wait()
	result.Msg, result.Ok = PostData(c.Endpoint(), "ping", file)
	return
}
