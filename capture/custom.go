package capture

import (
	"fmt"
	"os"

	"shell"
)

type Custom struct {
	Capture
	Index     int
	UrlParams string
	Command   []string
}

func (c *Custom) Run() (result Result, err error) {
	custom, err := os.Create(fmt.Sprintf("custom%d.out", c.Index))
	if err != nil {
		return
	}
	defer custom.Close()
	c.Cmd, err = shell.CommandStartInBackgroundToWriter(custom, c.Command)
	if err != nil {
		return
	}
	if c.Cmd.IsSkipped() {
		result.Msg = "skipped capturing Top"
		result.Ok = true
		return
	}
	c.Cmd.Wait()
	result.Msg, result.Ok = shell.PostCustomData(c.Endpoint(), c.UrlParams, custom)
	return
}
