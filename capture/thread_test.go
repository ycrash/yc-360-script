package capture

import (
	"testing"
)

func TestThread(t *testing.T) {
	t.Run("without-tdPath", func(t *testing.T) {
		td := &ThreadDump{
			Pid: 4085,
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
			Pid:    4085,
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
			Pid:    4085,
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
