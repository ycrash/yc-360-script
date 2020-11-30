package main

import "testing"

func TestZipFolder(t *testing.T) {
	err := zipFolder("zip")
	if err != nil {
		t.Fatal(err)
	}
}
