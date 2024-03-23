package capture

import (
	"shell/internal/utils"
	"testing"
)

func TestHDSub(t *testing.T) {
	noGC, err := utils.CommandStartInBackground(utils.Command{"java", "MyClass"})
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
