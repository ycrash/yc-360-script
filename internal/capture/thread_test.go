package capture

import (
	"testing"

	"yc-agent/internal/capture/executils"
)

func TestThread(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test requires Java and MyClass, makes external HTTP calls to gceasy.io endpoint.
	// Subtests: without-tdPath, with-invalid-tdPath are failing.
	// Needs Java environment setup and HTTP mocking for external calls.
	t.Skip("Skipping until Java environment and HTTP calls can be properly mocked")

	skipIfNoJava(t)

	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	t.Run("without-tdPath", func(t *testing.T) {
		td := &ThreadDump{
			Pid: noGC.GetPid(),
		}
		td.SetEndpoint(endpoint)
		result, err := td.Run()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Ok {
			t.Fatal(result)
		}
	})
	t.Run("with-tdPath", func(t *testing.T) {
		td := &ThreadDump{
			Pid:    noGC.GetPid(),
			TdPath: "threaddump-usr.out",
		}
		td.SetEndpoint(endpoint)
		result, err := td.Run()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Ok {
			t.Fatal(result)
		}
	})
	t.Run("with-invalid-tdPath", func(t *testing.T) {
		td := &ThreadDump{
			Pid:    noGC.GetPid(),
			TdPath: "threaddump-non.out",
		}
		td.SetEndpoint(endpoint)
		result, err := td.Run()
		if err != nil {
			t.Fatal(err)
		}
		if !result.Ok {
			t.Fatal(result)
		}
	})
}
