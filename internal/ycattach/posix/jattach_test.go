//go:build darwin || linux
// +build darwin linux

package posix

import (
	"testing"
	"time"

	"shell/internal"
)

func TestCaptureThreadDump(t *testing.T) {
	noGC, err := internal.CommandStartInBackground(internal.Command{"java", "-cp", "../../capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	time.Sleep(time.Second)
	ret := CaptureThreadDump(noGC.GetPid())
	t.Log(noGC.GetPid(), "ret", ret)
}
