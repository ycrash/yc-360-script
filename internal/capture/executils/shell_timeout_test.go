package executils

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHelperProcess is not a real test: the tests below run the test binary
// itself as the command, with a mode after "--", so they work on every OS.
func TestHelperProcess(t *testing.T) {
	mode := ""
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
		}
	}
	switch mode {
	case "":
		return // a normal test run
	case "print":
		fmt.Print("hello")
	case "fail":
		fmt.Print("bad input")
		os.Exit(3)
	case "sleep":
		time.Sleep(20 * time.Second)
		fmt.Print("too late")
	}
	os.Exit(0)
}

func helperCommand(mode string) Command {
	return Command{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode}
}

func withCmdTimeout(t *testing.T, d time.Duration) {
	original := config.GlobalConfig.CmdTimeout
	config.GlobalConfig.CmdTimeout = config.Duration(d)
	t.Cleanup(func() { config.GlobalConfig.CmdTimeout = original })
}

func TestCommandCombinedOutputToWriterOutput(t *testing.T) {
	withCmdTimeout(t, 30*time.Second)

	var out bytes.Buffer
	require.NoError(t, CommandCombinedOutputToWriter(&out, helperCommand("print")))
	assert.Contains(t, out.String(), "hello")

	// A failing command still reports its output, in the error and the writer.
	out.Reset()
	err := CommandCombinedOutputToWriter(&out, helperCommand("fail"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exit status 3 because bad input")
	assert.Contains(t, out.String(), "bad input")
	assert.False(t, errors.Is(err, ErrCommandTimeout))

	// A program that can't start returns the start error.
	err = CommandCombinedOutputToWriter(&out, Command{filepath.Join(t.TempDir(), "no-such-program")})
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrCommandTimeout))
}

func TestCommandCombinedOutputToWriterTimeout(t *testing.T) {
	withCmdTimeout(t, 500*time.Millisecond)

	for name, cmd := range map[string]Command{
		"command":      helperCommand("sleep"),
		"wait command": append(Command{WaitCommand}, helperCommand("sleep")...),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			start := time.Now()
			err := CommandCombinedOutputToWriter(&out, cmd)

			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrCommandTimeout), err)
			assert.Less(t, time.Since(start), 10*time.Second, "returns at the time limit, not when the command ends")

			// Nothing is written, now or later: the stopped command can't
			// reach the caller's writer after the function returned.
			time.Sleep(time.Second)
			assert.Empty(t, out.String())
		})
	}
}

func TestCommandCombinedOutputToWriterRunTime(t *testing.T) {
	// "print" exits at once; RunTime only raises the limit, it doesn't wait.
	withCmdTimeout(t, 500*time.Millisecond)
	var out bytes.Buffer
	require.NoError(t, CommandCombinedOutputToWriter(&out, helperCommand("print"), RunTime(time.Minute)))
	assert.Contains(t, out.String(), "hello")

	assert.Equal(t, 90*time.Second, runTimeOf([]Hooker{SudoHooker{}, RunTime(60 * time.Second), RunTime(30 * time.Second)}))
}

func TestCommandCombinedOutputToFileTimeout(t *testing.T) {
	withCmdTimeout(t, 500*time.Millisecond)
	name := filepath.Join(t.TempDir(), "out.txt")

	file, err := CommandCombinedOutputToFile(name, helperCommand("sleep"))
	assert.Nil(t, file, "no file, so callers fall back to their next method")
	assert.True(t, errors.Is(err, ErrCommandTimeout), err)
}

func TestCmdTimeoutDefault(t *testing.T) {
	withCmdTimeout(t, 0)
	assert.Equal(t, 60*time.Second, CmdTimeout())
}

func TestCommandName(t *testing.T) {
	cases := map[string]Command{
		"jstat":       {`D:\JDK\jdk-17.0.6/bin/jstat`, "-gc"},
		"tasklist":    {`C:\WINDOWS\system32\tasklist.exe`},
		"top":         {"/usr/bin/top", "-b"},
		"netstat":     {"netstat"},
		"yc":          {`C:\tools\yc.EXE`, "-tdCaptureMode"},
		"The command": nil,
		"PowerShell":  {WaitCommand, `C:\WINDOWS\System32\WindowsPowerShell\v1.0\PowerShell.exe`, "-Command", "systeminfo"},
		"uname":       {WaitCommand, "uname", "-a"},
	}
	for want, cmd := range cases {
		assert.Equal(t, want, commandName(cmd), cmd)
	}
}

func TestTimeoutMessage(t *testing.T) {
	cmd := Command{`C:\WINDOWS\system32\tasklist.exe`}
	assert.Equal(t, `tasklist was stopped because it ran longer than -cmdTimeout (1s), so its data was not captured. Increase -cmdTimeout to give it more time. Command: C:\WINDOWS\system32\tasklist.exe`,
		timeoutMessage(cmd, 0, time.Second, `C:\WINDOWS\system32\tasklist.exe`))

	jstat := Command{`D:\JDK\jdk-17.0.6/bin/jstat`, "-gc", "-t", "222624", "2000", "30"}
	assert.Equal(t, `jstat was stopped because it ran longer than its 1m0s run time plus -cmdTimeout (1s), so its data was not captured. Increase -cmdTimeout to give it more time. Command: D:\JDK\jdk-17.0.6/bin/jstat -gc -t 222624 2000 30`,
		timeoutMessage(jstat, time.Minute, time.Second, `D:\JDK\jdk-17.0.6/bin/jstat -gc -t 222624 2000 30`))
}
