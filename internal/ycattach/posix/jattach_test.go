//go:build darwin || linux
// +build darwin linux

package posix

import (
	"shell"
	"testing"
	"time"
)

func TestCaptureThreadDump(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "-cp", "../../capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	time.Sleep(time.Second)
	ret := CaptureThreadDump(noGC.GetPid())
	t.Log(noGC.GetPid(), "ret", ret)
}
