package capture

import (
	"testing"
	"yc-agent/internal/capture/executils"
)

func TestCheckProcessExists(t *testing.T) {
	e := IsProcessExists(65535)
	if e {
		t.Fatal("process 65535 should not exists")
	}

	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()

	e = IsProcessExists(noGC.GetPid())
	if !e {
		t.Fatal("process should be exists")
	}
}
