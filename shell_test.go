package shell

import (
	"os/exec"
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

func TestPS(t *testing.T) {
	cmd := exec.Command("PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}")
	bytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", bytes)
}
