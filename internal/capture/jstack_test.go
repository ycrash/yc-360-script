package capture

import (
	"os"
	"testing"

	"yc-agent/internal/capture/executils"
)

func TestJStack(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test requires Java and MyClass to be available. Even with skipIfNoJava,
	// the test may fail due to missing test class files or Java environment issues.
	t.Skip("Skipping until Java environment and MyClass can be properly set up in CI")

	skipIfNoJava(t)

	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "MyClass"})
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
	skipIfNoJava(t)
	t.Skip(" -F option used. Cannot connect to core dump or remote debug server. Use jhsdb jstack instead")
	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "MyClass"})
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
