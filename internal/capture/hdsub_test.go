package capture

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"yc-agent/internal/capture/executils"
)

func TestHDSub(t *testing.T) {
	// Create temporary directory for test execution
	tmpDir, err := os.MkdirTemp("", "hdsub-capture-test-*")
	require.NoError(t, err, "Failed to create temp directory")
	defer os.RemoveAll(tmpDir)

	// Change to temp directory for test execution
	originalDir, err := os.Getwd()
	require.NoError(t, err, "Failed to get current directory")
	defer os.Chdir(originalDir)

	err = os.Chdir(tmpDir)
	require.NoError(t, err, "Failed to change to temp directory")

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
