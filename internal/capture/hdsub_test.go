package capture

import (
	"testing"
	"yc-agent/internal/capture/executils"
)

func TestHDSub(t *testing.T) {
	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	cap := &HDSub{JavaHome: javaHome, Pid: noGC.GetPid()}
	_, err = cap.Run()
	if err != nil {
		t.Fatal(err)
	}
}
