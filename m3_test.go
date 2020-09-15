package shell

import (
	"testing"

	"shell/config"
)

func TestGetProcessIds(t *testing.T) {
	ids, err := GetProcessIds(config.ProcessTokens{"sharingd", "nsurlstoraged"})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(ids)
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
