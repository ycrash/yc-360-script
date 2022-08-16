package capture

import (
	"fmt"
	"strings"
	"sync"

	"shell"
	"shell/logger"
)

type Result struct {
	Msg string
	Ok  bool
}

type Capture struct {
	Cmd      shell.CmdManager
	endpoint string
	wg       sync.WaitGroup
}

func (cap *Capture) DoneWaitGroup() {
	cap.wg.Done()
}

func (cap *Capture) InitWaitGroup() {
	cap.wg.Add(1)
}

func (cap *Capture) WaitWaitGroup() {
	if cap.Cmd == nil {
		return
	}
	cap.wg.Wait()
}

func (cap *Capture) Interrupt() error {
	if cap.Cmd == nil {
		return nil
	}
	return cap.Cmd.Interrupt()
}

func (cap *Capture) Kill() error {
	if cap.Cmd == nil {
		return nil
	}
	return cap.Cmd.Kill()
}

func (cap *Capture) Endpoint() string {
	return cap.endpoint
}

func (cap *Capture) EndpointWithParams(params string) string {
	if strings.Contains(cap.endpoint, "?") {
		return fmt.Sprintf("%s&%s", cap.endpoint, params)
	} else {
		return fmt.Sprintf("%s?%s", cap.endpoint, params)
	}
}

func (cap *Capture) SetEndpoint(endpoint string) {
	cap.endpoint = endpoint
}

type Task interface {
	SetEndpoint(endpoint string)
	Run() (result Result, err error)
	Kill() error
	InitWaitGroup()
	DoneWaitGroup()
	WaitWaitGroup()
}

func WrapRun(task Task) func(endpoint string, c chan Result) {
	return func(endpoint string, c chan Result) {
		var err error
		var result Result
		defer func() {
			if err != nil {
				logger.Log("capture %#v failed: %+v", task, err)
				result.Msg = fmt.Sprintf("capture failed: %s", err.Error())
			}
			c <- result
			close(c)
			task.DoneWaitGroup()
		}()
		task.SetEndpoint(endpoint)
		task.InitWaitGroup()
		result, err = task.Run()
	}
}

func (cap *Capture) Run() (result Result, err error) {
	return
}

func GoCapture(endpoint string, fn func(endpoint string, c chan Result), wait ...Task) (c chan Result) {
	c = make(chan Result)
	go func() {
		for _, task := range wait {
			task.WaitWaitGroup()
		}
		fn(endpoint, c)
	}()
	return
}
