package capture

import (
	"fmt"

	"shell"
	"shell/logger"
)

type Result struct {
	Msg string
	Ok  bool
}

type Capture struct {
	Cmd      shell.CmdHolder
	endpoint string
}

func (cap *Capture) Interrupt() error {
	return cap.Cmd.Interrupt()
}

func (cap *Capture) Kill() error {
	return cap.Cmd.Kill()
}

func (cap *Capture) Endpoint() string {
	return cap.endpoint
}

func (cap *Capture) SetEndpoint(endpoint string) {
	cap.endpoint = endpoint
}

type Task interface {
	SetEndpoint(endpoint string)
	Run() (result Result, err error)
	Kill() error
}

func WrapRun(task Task) func(endpoint string, c chan Result) {
	return func(endpoint string, c chan Result) {
		var err error
		var result Result
		defer func() {
			if err != nil {
				logger.Log("capture failed: %+v", err)
				result.Msg = fmt.Sprintf("capture failed: %s", err.Error())
			}
			c <- result
			close(c)
		}()
		task.SetEndpoint(endpoint)
		result, err = task.Run()
	}
}

func (cap *Capture) Run() (result Result, err error) {
	return
}

func GoCapture(endpoint string, fn func(endpoint string, c chan Result)) (c chan Result) {
	c = make(chan Result)
	go fn(endpoint, c)
	return
}
