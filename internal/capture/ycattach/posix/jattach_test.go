//go:build darwin || linux
// +build darwin linux

package posix

import (
	"testing"
	"time"

	"yc-agent/internal/capture/executils"
)

func TestCaptureThreadDump(t *testing.T) {
	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "-cp", "../../capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	time.Sleep(time.Second)
	ret := CaptureThreadDump(noGC.GetPid())
	t.Log(noGC.GetPid(), "ret", ret)
}
