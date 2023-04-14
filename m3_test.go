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
	ids, err := GetProcessIds(config.ProcessTokens{"MyClass$appNameTest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
	if len(ids) < 1 {
		t.Fatal("can not get pid of java process")
	}
}

func TestParseJsonResp(t *testing.T) {
	ids, tags, _, err := ParseJsonResp([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"] }`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, tags)

	ids, tags, _, err = ParseJsonResp([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, tags)

	ids, tags, _, err = ParseJsonResp([]byte(`{ "actions": [] }`))
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

	ids, err := GetProcessIds(config.ProcessTokens{config.ProcessToken(fmt.Sprintf("%d$appNameTest", noGC.GetPid()))}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
	if name, ok := ids[noGC.GetPid()]; !ok || name != "appNameTest" {
		t.Fatal("can not get pid of java process")
	}
}

func TestGetProcessIdsWithExclude(t *testing.T) {
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

	ids, err := GetProcessIds(config.ProcessTokens{"MyClass$appNameTest"}, config.ProcessTokens{"wait"})
	//ids, err := GetProcessIds(config.ProcessTokens{"MyClass$appNameTest"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids, noGC.GetPid())
	if name, ok := ids[noGC.GetPid()]; !ok || name != "appNameTest" {
		t.Fatal("can not get pid of java process")
	}
}
