package capture

import (
	"fmt"
	"os"

	"shell"
)

type PS struct {
	Capture
	c chan struct{}
}

func NewPS() *PS {
	p := &PS{c: make(chan struct{})}
	return p
}

func (t *PS) Run() (result Result, err error) {
	ps, err := os.Create("ps.out")
	if err != nil {
		return
	}
	defer ps.Close()

	for {
		_, ok := <-t.c
		if !ok {
			break
		}
		_, err = ps.WriteString(fmt.Sprintf("\n%s\n", shell.NowString()))
		if err != nil {
			return
		}
		err = shell.CommandCombinedOutputToWriter(ps, shell.PS)
		if err != nil {
			return
		}
	}
	result.Msg, result.Ok = shell.PostData(t.endpoint, "ps", ps)
	return
}

func (t *PS) Continue() (ok bool) {
	select {
	case t.c <- struct{}{}:
		ok = true
	default:
	}
	return
}

func (t *PS) Kill() (err error) {
	close(t.c)
	return
}
