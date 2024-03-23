package capture

import (
	"os"

	"shell/internal/utils"
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
	c.Cmd, err = utils.CommandStartInBackgroundToWriter(file, utils.Append(utils.Ping, c.Host))
	if err != nil {
		return
	}
	if c.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Ping"
		result.Ok = false
		return
	}
	c.Cmd.Wait()
	result.Msg, result.Ok = utils.PostData(c.Endpoint(), "ping", file)
	return
}
