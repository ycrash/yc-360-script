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
	for n := 1; n <= count; n++ {
		//  Collect a javacore against the problematic pid (passed in by the user)
		//  Javacores are output to the working directory of the JVM; in most cases this is the <profile_root>
		err = func() error {
			jstack, err := shell.CommandCombinedOutputToFile(fmt.Sprintf("javacore.%d.out", n),
				shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)})
			if err != nil {
				return err
			}
			defer jstack.Close()

			_, err = JStackF{
				jstack:   jstack,
				javaHome: t.javaHome,
				pid:      t.pid,
			}.Run()
			return err
		}()
		if err != nil {
			return
		}
		topH := TopH{Pid: t.pid, N: n}
		result, err = topH.Run()
		if err != nil {
			return
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
