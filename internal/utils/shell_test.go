package utils

import (
	"testing"
)

func TestNilCmdHolder(t *testing.T) {
	cmdHolder := Cmd{}
	defer func() {
		if err := recover(); err != nil {
			t.Fatal(err)
		}
	}()
	cmdHolder.Wait()
}

func TestCaptureCmd(t *testing.T) {
	_, err := RunCaptureCmd(123, "echo $pid")
	if err != nil {
		t.Fatal(err)
	}
}
