package shell

import (
	"fmt"
	"strconv"
	"testing"

	"shell/config"

	"github.com/stretchr/testify/assert"
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

func TestParseJsonRespLegacy(t *testing.T) {
	ids, _, ts, err := ParseJsonResp([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamp": "2023-05-05T20-23-23"}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, ts, []string{"2023-05-05T20-23-23"})

	ids, _, ts, err = ParseJsonResp([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, _, err = ParseJsonResp([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
}

func TestParseJsonResp(t *testing.T) {
	ids, _, ts, err := ParseJsonResp([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamps": ["2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, []string{"2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"}, ts)

	ids, _, ts, err = ParseJsonResp([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, _, err = ParseJsonResp([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
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
