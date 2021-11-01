package capture

import (
	"shell"
	"testing"
)

func TestHDSub(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
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
