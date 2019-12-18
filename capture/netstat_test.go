package capture

import "testing"

func TestNetStat(t *testing.T) {
	capNetStat := NewNetStat()
	capNetStat.SetEndpoint(endpoint)
	go func() {
		capNetStat.Done()
	}()
	result, err := capNetStat.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
