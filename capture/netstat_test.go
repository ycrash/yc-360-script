package capture

import (
	"os"
	"testing"
)

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

func TestNativeNetStat(t *testing.T) {
	err := netStat(true, true, true, true, true, true, false, os.Stdout)
	if err != nil {
		t.Fatal(err)
	}
}
