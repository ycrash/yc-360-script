package capture

import (
	"os"
	"testing"

	"shell/internal"
)

func TestJStack(t *testing.T) {
	noGC, err := internal.CommandStartInBackground(internal.Command{"java", "MyClass"})
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

// -F option used
//
//	Cannot connect to core dump or remote debug server. Use jhsdb jstack instead
func TestJStackF_Run(t *testing.T) {
	t.Skip(" -F option used. Cannot connect to core dump or remote debug server. Use jhsdb jstack instead")
	noGC, err := internal.CommandStartInBackground(internal.Command{"java", "MyClass"})
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
