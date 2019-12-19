package capture

import (
	"testing"
	"time"

	"shell"
)

func TestJStack(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	capJStack := NewJStack(javaHome, noGC.Process.Pid)
	go func() {
		time.Sleep(time.Second)
		capJStack.Continue()
		capJStack.Kill()
	}()
	_, err = capJStack.Run()
	if err != nil {
		t.Fatal(err)
	}
}
