//go:build windows
// +build windows

package utils

import (
	"os/exec"
	"testing"
)

func TestPS(t *testing.T) {
	cmd := exec.Command("PowerShell.exe", "-Command", "& {ps | sort -desc cpu | select -first 30}")
	bytes, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s", bytes)
}
