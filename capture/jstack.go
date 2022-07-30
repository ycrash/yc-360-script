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
	b1 := make(chan int, count)
	b2 := make(chan int, count)
	e1 := make(chan error, count)
	e2 := make(chan error, count)
	defer func() {
		close(b1)
		close(b2)
	}()
	go func() {
		defer func() {
			close(e1)
		}()
		for {
			n, ok := <-b1
			if !ok {
				return
			}
			fn := fmt.Sprintf("javacore.%d.out", n)
			jstack, err := shell.CommandCombinedOutputToFile(fn,
				shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
			if err != nil {
				logger.Log("Failed to run jstack with err %v. Trying to capture thread dump using jattach...", err)
				jstack, err = shell.CommandCombinedOutputToFile(fn,
					shell.Command{shell.Executable(), "-p", strconv.Itoa(t.pid), "-tdCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
				if err != nil {
					logger.Log("Failed to run jattach with err %v. Trying to capture thread dump using jattach in temp path...", err)
					tempPath, err := shell.Copy2TempPath()
					if err != nil {
						e1 <- err
						return
					}
					jstack, err = shell.CommandCombinedOutputToFile(fn,
						shell.Command{tempPath, "-p", strconv.Itoa(t.pid), "-tdCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
					if err != nil {
						e1 <- err
						return
					}
				}
			}
			e := jstack.Sync()
			if e != nil {
				logger.Log("failed to sync file %s", e)
			}
			_, e = jstack.WriteString("\nFull thread dump\n")
			if e != nil {
				logger.Log("failed to write file %s", e)
			}
			_, err = (&JStackF{
				jstack:   jstack,
				javaHome: t.javaHome,
				pid:      t.pid,
			}).Run()
			e1 <- err

			e = jstack.Sync()
			if e != nil {
				logger.Log("failed to sync file %s", e)
			}
			e = jstack.Close()
			if e != nil {
				logger.Log("failed to close file %s", e)
			}
		}
	}()

	go func() {
		defer func() {
			close(e2)
		}()
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
			logger.Warn().Err(err).Msg("Failed to run jstack with err")
		}
		err = <-e2
		if err != nil {
			logger.Warn().Err(err).Msg("Failed to run top h with err")
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

func (t *JStackF) Run() (result Result, err error) {
	_, err = t.jstack.Seek(0, 0)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(t.jstack)
	i := 0
	for scanner.Scan() && i <= 5 {
		i++
	}

	if i <= 5 {
		_, err = t.jstack.Seek(0, 0)
		if err != nil {
			return
		}
		err = shell.CommandCombinedOutputToWriter(t.jstack,
			shell.Command{path.Join(t.javaHome, "bin/jstack"), "-F", strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
		if err != nil {
			err = shell.CommandCombinedOutputToWriter(t.jstack,
				shell.Command{shell.Executable(), "-p", strconv.Itoa(t.pid), "-tdCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
		}
	}
	return
}
