package capture

import (
	"os"
	"strconv"
	"testing"
	"time"

	"shell/internal/utils"
)

func TestTop(t *testing.T) {
	top := &Top{}
	top.SetEndpoint(endpoint)
	result, err := top.Run()
	if err != nil {
		t.Fatal(err)
	}
	if !result.Ok {
		t.Fatal(result)
	}
}

func TestTopH(t *testing.T) {
	noGC, err := utils.CommandStartInBackground(utils.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	top := &TopH{
		Pid: noGC.GetPid(),
	}
	go func() {
		time.Sleep(time.Second)
		top.Kill()
	}()
	_, err = top.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTop4M3(t *testing.T) {
	top := &Top4M3{}
	_, err := top.Run()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSkip(t *testing.T) {
	command, err := utils.NopCommand.AddDynamicArg(strconv.Itoa(100))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%#v", command)
	c, err := utils.CommandStartInBackgroundToWriter(os.NewFile(0, os.DevNull), command)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%#v", c)
	if !c.IsSkipped() {
		t.Fatal("failed to skip")
	}
}

func TestNop(t *testing.T) {
	var a = utils.NopCommand
	if len(a) > 0 {
		t.Fatal("")
	}
}
