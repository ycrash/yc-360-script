package capture

import (
	"testing"
)

func TestVMStat(t *testing.T) {
	v := &VMStat{}
	v.SetEndpoint(endpoint)
	result, err := v.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
