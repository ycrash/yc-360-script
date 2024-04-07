package ondemand

import "testing"

func TestZipFolder(t *testing.T) {
	_, err := ZipFolder("zip")
	if err != nil {
		t.Fatal(err)
	}
}
