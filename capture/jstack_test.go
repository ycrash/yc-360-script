package capture

import (
	"os"
	"testing"

	"shell"
)

func TestJStack(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	capJStack := NewJStack(javaHome, noGC.GetPid())
	_, err = capJStack.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func TestJStackF_Run(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	file, err := os.Open("jstackf.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	j := JStackF{
		jstack:   file,
		javaHome: javaHome,
		pid:      noGC.GetPid(),
	}
	_, err = j.Run()
	if err != nil {
		t.Fatal(err)
	}
}
