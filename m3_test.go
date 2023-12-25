package shell

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseJsonRespLegacy(t *testing.T) {
	ids, _, ts, err := ParseM3FinResponse([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamp": "2023-05-05T20-23-23"}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, ts, []string{"2023-05-05T20-23-23"})

	ids, _, ts, err = ParseM3FinResponse([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, _, err = ParseM3FinResponse([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
}

func TestParseJsonResp(t *testing.T) {
	ids, _, ts, err := ParseM3FinResponse([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamps": ["2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, []string{"2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"}, ts)

	ids, _, ts, err = ParseM3FinResponse([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, _, err = ParseM3FinResponse([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
}
