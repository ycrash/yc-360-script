package capture

import (
	"testing"

	"shell"
)

func TestJStack(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	capJStack := NewJStack(javaHome, noGC.Process.Pid)
	_, err = capJStack.Run()
	if err != nil {
		t.Fatal(err)
	}
}
