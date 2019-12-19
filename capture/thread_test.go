package capture

import (
	"testing"
)

func TestThread(t *testing.T) {
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
}
