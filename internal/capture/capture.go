package capture

import (
	"fmt"
	"strings"
	"sync"

	"shell/internal/capture/executils"
	"shell/internal/logger"
)

type Result struct {
	Msg string
	Ok  bool
}

type Capture struct {
	Cmd               executils.CmdManager
	endpoint          string
	wg                sync.WaitGroup
	mapEndpointParams map[string]string
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
	if len(cap.mapEndpointParams) == 0 {
		return cap.endpoint
	}

	return cap.getEndpointWithParams()
}

func (cap *Capture) getEndpointWithParams() string {
	pairs := make([]string, len(cap.mapEndpointParams))
	for key, value := range cap.mapEndpointParams {
		pairs = append(pairs, key+"="+value)
	}
	endpointParams := strings.Join(pairs, "&")
	endpointDelimiter := "?"
	if strings.Contains(cap.endpoint, "?") {
		endpointDelimiter = "&"
	}
	return fmt.Sprintf("%s%s%s", cap.endpoint, endpointDelimiter, endpointParams)
}

func (cap *Capture) SetEndpoint(endpoint string) {
	cap.endpoint = endpoint
}

func (cap *Capture) SetEndpointParam(name, value string) {
	if cap.mapEndpointParams == nil {
		cap.mapEndpointParams = make(map[string]string)
	}
	cap.mapEndpointParams[name] = value
}

func (cap *Capture) RemoveEndpointParam(name string) {
	if cap.mapEndpointParams == nil {
		return
	}
	delete(cap.mapEndpointParams, name)
}

type Task interface {
	SetEndpoint(endpoint string)
	SetEndpointParam(name, value string)
	RemoveEndpointParam(name string)
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
