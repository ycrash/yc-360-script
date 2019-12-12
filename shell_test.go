package shell

import (
	"testing"
)

func TestNilCmdHolder(t *testing.T) {
	cmdHolder := CmdHolder{}
	defer func() {
		if err := recover(); err != nil {
			t.Fatal(err)
		}
	}()
	cmdHolder.Wait()
}
