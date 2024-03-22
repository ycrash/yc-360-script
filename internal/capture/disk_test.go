package capture

import "testing"

func TestDisk(t *testing.T) {
	d := &Disk{}
	d.SetEndpoint(endpoint)
	result, err := d.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}
