package capture

import (
	"fmt"
	"path"
	"strconv"

	"shell"
)

type JStack struct {
	Capture
	c        chan struct{}
	javaHome string
	pid      int
}

func NewJStack(javaHome string, pid int) *JStack {
	return &JStack{c: make(chan struct{}), javaHome: javaHome, pid: pid}
}

func (t *JStack) Run() (result Result, err error) {
	for n := 1; true; n++ {
		_, ok := <-t.c
		if !ok {
			break
		}
		jstack, err := shell.CommandCombinedOutputToFile(fmt.Sprintf("javacore.%d.out", n),
			shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)})
		if err != nil {
			return result, err
		}
		jstack.Close()
	}
	return
}

func (t *JStack) Continue() (ok bool) {
	select {
	case t.c <- struct{}{}:
		ok = true
	default:
	}
	return
}

func (t *JStack) Kill() (err error) {
	close(t.c)
	return
}
