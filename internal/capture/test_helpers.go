package capture

import (
	"os"
	"os/exec"
	"testing"
)

// skipIfNoJava skips the test if Java is not available in the environment.
// This prevents tests from failing when Java/JDK tools are not installed.
func skipIfNoJava(t *testing.T) {
	if os.Getenv("JAVA_HOME") == "" {
		t.Skip("JAVA_HOME not set, skipping Java-dependent test")
	}

	if _, err := exec.LookPath("java"); err != nil {
		t.Skip("java not found in PATH, skipping test")
	}
}
