package shell

import (
	"fmt"
	"strconv"
	"testing"

	"shell/config"
)

func TestGetProcessIds(t *testing.T) {
	noGC, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	ids, err := GetProcessIds(config.ProcessTokens{"MyClass$appNameTest"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
	if len(ids) < 1 {
		t.Fatal("can not get pid of java process")
	}
}

func TestParseJsonResp(t *testing.T) {
	ids, tags, err := ParseJsonResp([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"] }`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, tags)

	ids, tags, err = ParseJsonResp([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, tags)

	ids, tags, err = ParseJsonResp([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, tags)
}

func TestGetProcessIdsByPid(t *testing.T) {
	noGC, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()

	fake, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass", "-wait", strconv.Itoa(noGC.GetPid())})
	if err != nil {
		t.Fatal(err)
	}
	defer fake.KillAndWait()

	ids, err := GetProcessIds(config.ProcessTokens{config.ProcessToken(fmt.Sprintf("%d$appNameTest", noGC.GetPid()))})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
	if len(ids) != 1 {
		t.Fatal("can not get pid of java process")
	}
}
