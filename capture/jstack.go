package capture

import (
	"fmt"
	"path"
	"strconv"
	"time"

	"shell"
	"shell/logger"
)

type JStack struct {
	Capture
	javaHome string
	pid      int
}

func NewJStack(javaHome string, pid int) *JStack {
	return &JStack{javaHome: javaHome, pid: pid}
}

func (t *JStack) Run() (result Result, err error) {
	m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		//  Collect a javacore against the problematic pid (passed in by the user)
		//  Javacores are output to the working directory of the JVM; in most cases this is the <profile_root>
		jstack, err := shell.CommandCombinedOutputToFile(fmt.Sprintf("javacore.%d.out", n),
			shell.Command{path.Join(t.javaHome, "bin/jstack"), "-l", strconv.Itoa(t.pid)})
		if err != nil {
			return result, err
		}
		jstack.Close()

		if n == m {
			break
		}
		// Pause for JAVACORE_INTERVAL seconds.
		logger.Log("sleeping for %d seconds...", shell.JAVACORE_INTERVAL)
		time.Sleep(time.Second * time.Duration(shell.JAVACORE_INTERVAL))
	}
	return
}
