package capture

import (
	"testing"
	"time"
)

func TestVMStat(t *testing.T) {
	v := &VMStat{}
	v.SetEndpoint(endpoint)
	go func() {
		time.Sleep(time.Second)
		v.Kill()
	}()
	result, err := v.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
