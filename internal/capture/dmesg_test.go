package capture

import (
	"testing"
)

func TestDMesg(t *testing.T) {
	d := &DMesg{}
	d.SetEndpoint(endpoint)
	result, err := d.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
