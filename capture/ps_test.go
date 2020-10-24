package capture

import (
	"testing"
)

func TestPS(t *testing.T) {
	ps := NewPS()
	ps.SetEndpoint(endpoint)
	go func() {
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
