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
			outputFileName := fmt.Sprintf("javacore.%d.out", n)
			var jstackFile *os.File = nil

			// Thread dump: Attempt 1: jstack
			if jstackFile == nil {
				logger.Log("Trying to capture thread dump using jstack ...")
				jstackFile, err = shell.CommandCombinedOutputToFile(
					outputFileName,
					shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)},
					shell.SudoHooker{PID: t.pid},
				)
				if err != nil {
					logger.Log("Failed to run jstack with err %v", err)
				}
			}
			//  Thread dump: Attempt 2a: jattach via self execution with -tdCaptureMode
			if jstackFile == nil {
				logger.Log("Trying to capture thread dump using jattach...")
				jstackFile, err = shell.CommandCombinedOutputToFile(outputFileName,
					shell.Command{shell.Executable(), "-p", strconv.Itoa(t.pid), "-tdCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
				if err != nil {
					logger.Log("Failed to run jattach with err %v", err)
				}
			}

			// Thread dump: Attempt 2b: jattach via self execution from tmp path with -tdCaptureMode
			if jstackFile == nil {
				logger.Log("Trying to capture thread dump using jattach in temp path...")
				tempPath, err := shell.Copy2TempPath()
				if err == nil {
					jstackFile, err = shell.CommandCombinedOutputToFile(outputFileName,
						shell.Command{tempPath, "-p", strconv.Itoa(t.pid), "-tdCaptureMode"}, shell.EnvHooker{"pid": strconv.Itoa(t.pid)}, shell.SudoHooker{PID: t.pid})
					if err != nil {
						logger.Log("Failed to run jattach with err %v", err)
					}
				} else {
					logger.Log("Failed to Copy2TempPath with err %v", err)
				}
			}

			// Thread dump: Attempt 5: jstack -F
			if jstackFile == nil {
				logger.Log("Trying to capture thread dump using jstack -F ...")
				jstackFile, err = os.Create(outputFileName)
				if err != nil {
					logger.Log("Failed to create output file %v", err)
					e1 <- err
					return
				}

				_, e := jstackFile.WriteString("\nFull thread dump\n")
				if e != nil {
					logger.Log("failed to write file %s", e)
					e1 <- e
					_ = jstackFile.Close()
					return
				}
				_, err = (&JStackF{
					jstack:   jstackFile,
					javaHome: t.javaHome,
					pid:      t.pid,
				}).Run()
				if err != nil {
					logger.Log("failed to collect dump using jstack -F : %v", err)
					e1 <- err
					_ = jstackFile.Close()
					return
				}
			}

			// Thread dump: Attempt 6: jhsdb jstack --pid PID
			// If you see this error:
			// java.lang.RuntimeException: Unable to deduce type of thread from address 0x00007fab10001000 (expected type JavaThread, CompilerThread, ServiceThread, JvmtiAgentThread or CodeCacheSweeperThread)
			// It requires the debug information. In ubuntu, you can install it with: apt install openjdk-11-dbg
			if jstackFile == nil {
				logger.Log("Trying to capture thread dump using jhsdb jstack ...")

				jstackFile, err = os.Create(outputFileName)
				if err != nil {
					logger.Log("Failed to create output file %v", err)
					e1 <- err
					return
				}

				_, e := jstackFile.WriteString("\nFull thread dump\n")
				if e != nil {
					logger.Log("failed to write file %s", e)
					e1 <- e
					_ = jstackFile.Close()
					return
				}

				err = shell.CommandCombinedOutputToWriter(jstackFile,
					shell.Command{path.Join(t.javaHome, "bin/jhsdb"), "jstack", "--pid", strconv.Itoa(t.pid)},
					shell.SudoHooker{PID: t.pid},
				)

				if err != nil {
					logger.Log("Failed to run jhsdb jstack with err %v", err)
				}
			}

			var e error
			if jstackFile != nil {
				e := jstackFile.Sync()
				if e != nil {
					logger.Log("failed to sync file %v", e)
				}
				_ = jstackFile.Close()
			}

			// necessary to send something into channel to prevent blocking inside waiting loop
			e1 <- e
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
			logger.Warn().Err(err).Msg("Failed to gather thread dump with err")
		}
		err = <-e2
		if err != nil {
			logger.Warn().Err(err).Msg("Failed to run top h with err")
		}

		if n < count {
			logger.Log("sleeping for %v for next capture of thread dump ...", timeToSleep)
			time.Sleep(timeToSleep)
		}
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
