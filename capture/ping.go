package capture

import (
	"os"
	"shell"
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
	c.Cmd, err = shell.CommandStartInBackgroundToWriter(file, shell.Ping, c.Host)
	if err != nil {
		return
	}
	if c.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Ping"
		result.Ok = true
		return
	}
	c.Cmd.Wait()
	result.Msg, result.Ok = shell.PostData(c.Endpoint(), "ping", file)
	return
}
