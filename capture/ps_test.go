package capture

import (
	"testing"
	"time"
)

func TestPS(t *testing.T) {
	ps := NewPS()
	ps.SetEndpoint(endpoint)
	go func() {
		time.Sleep(time.Second)
		ps.Continue()
		ps.Kill()
	}()
	result, err := ps.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
