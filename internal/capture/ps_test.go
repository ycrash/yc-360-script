package capture

import (
	"shell/internal/config"
	"testing"
)

func TestPS(t *testing.T) {
	ps := NewPS()
	ps.SetEndpoint(endpoint)
	config.GlobalConfig.ApiKey = "e094a34e-c3eb-4c9a-8254-f0dd107245c"
	result, err := ps.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
	t.Log(result)
}
