package internal

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
