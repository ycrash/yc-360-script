package capture

import (
	"testing"

	"shell/internal/utils"
)

func TestThread(t *testing.T) {
	noGC, err := utils.CommandStartInBackground(utils.Command{"java", "MyClass"})
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
