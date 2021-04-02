package capture

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strconv"
	"time"

	"shell"
	"shell/logger"
)

const count = 3
const timeToSleep = 10 * time.Second

type JStack struct {
	Capture
	javaHome string
	pid      int
}

func NewJStack(javaHome string, pid int) *JStack {
	return &JStack{javaHome: javaHome, pid: pid}
}

func (t *JStack) Run() (result Result, err error) {
	b1 := make(chan int, 1)
	b2 := make(chan int, 1)
	e1 := make(chan error, 1)
	e2 := make(chan error, 1)
	defer func() {
		close(b1)
		close(b2)
	}()
	go func() {
		for {
			n, ok := <-b1
			if !ok {
				return
			}
			jstack, err := shell.CommandCombinedOutputToFile(fmt.Sprintf("javacore.%d.out", n),
				shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)})
			if err != nil {
				e1 <- err
			}
			defer jstack.Close()

			_, err = JStackF{
				jstack:   jstack,
				javaHome: t.javaHome,
				pid:      t.pid,
			}.Run()
			e1 <- err
		}
	}()

	go func() {
		for {
			n, ok := <-b2
			if !ok {
				return
			}
			topH := TopH{Pid: t.pid, N: n}
			_, err = topH.Run()
			e2 <- err
		}
	}()

	for n := 1; n <= count; n++ {
		b2 <- n
		b1 <- n
		err = <-e1
		if err != nil {
			break
		}
		err = <-e2
		if err != nil {
			break
		}

		if n == count {
			break
		}
		logger.Log("sleeping for %v for next capture of jstack...", timeToSleep)
		time.Sleep(timeToSleep)
	}
	return
}

type JStackF struct {
	Capture
	jstack   *os.File
	javaHome string
	pid      int
}

func (t JStackF) Run() (result Result, err error) {
	t.jstack.Seek(0, 0)
	scanner := bufio.NewScanner(t.jstack)
	i := 0
	for scanner.Scan() && i <= 5 {
		i++
	}

	if i <= 5 {
		t.jstack.Seek(0, 0)
		err = shell.CommandCombinedOutputToWriter(t.jstack,
			shell.Command{path.Join(t.javaHome, "bin/jstack"), "-F", strconv.Itoa(t.pid)})
	}
	return
}
