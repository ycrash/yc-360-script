package main

import "testing"

func TestZipFolder(t *testing.T) {
	_, err := zipFolder("zip")
	if err != nil {
		t.Fatal(err)
	}
}
