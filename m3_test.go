package shell

import (
	"testing"

	"shell/config"
)

func TestGetProcessIds(t *testing.T) {
	noGC, err := CommandStartInBackground(Command{"java", "-cp", "./capture/testdata/", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()
	ids, err := GetProcessIds(config.ProcessTokens{"MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
	if len(ids) < 1 {
		t.Fatal("can not get pid of java process")
	}
}

func TestParseJsonResp(t *testing.T) {
	ids, err := ParseJsonResp([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"] }`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)

	ids, err = ParseJsonResp([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)

	ids, err = ParseJsonResp([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
}
