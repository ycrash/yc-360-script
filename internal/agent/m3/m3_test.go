package m3

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestM3FinPids(t *testing.T) {
	var a = func(pids []int) string {
		if len(pids) == 0 {
			return ""
		}
		var ps strings.Builder
		i := 0
		for ; i < len(pids)-1; i++ {
			ps.WriteString(strconv.Itoa(pids[i]))
			ps.WriteString("-")
		}
		ps.WriteString(strconv.Itoa(pids[i]))
		return ps.String()
	}

	t.Run("0", func(t *testing.T) {
		r := a(nil)
		t.Log(r)
	})
	t.Run("1", func(t *testing.T) {
		r := a([]int{1})
		t.Log(r)
	})
	t.Run("2", func(t *testing.T) {
		r := a([]int{1, 2})
		t.Log(r)
	})
	t.Run("2", func(t *testing.T) {
		r := a([]int{1, 2, 3})
		t.Log(r)
	})
}

func TestParseJsonRespLegacy(t *testing.T) {
	ids, _, ts, _, err := ParseM3FinResponse([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamp": "2023-05-05T20-23-23"}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, ts, []string{"2023-05-05T20-23-23"})

	ids, _, ts, _, err = ParseM3FinResponse([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, ts, _, err = ParseM3FinResponse([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
	assert.Equal(t, []string{}, ts)
}

func TestParseJsonResp(t *testing.T) {
	ids, _, ts, _, err := ParseM3FinResponse([]byte(`{"actions":[ "capture 12321", "capture 2341", "capture 45321"], "timestamps": ["2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{12321, 2341, 45321}, ids)
	assert.Equal(t, []string{"2023-05-05T20-23-23", "2023-05-05T20-23-24", "2023-05-05T20-23-25"}, ts)

	ids, _, ts, _, err = ParseM3FinResponse([]byte(`{"actions":["capture 2116"]}`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{2116}, ids)
	assert.Equal(t, []string{}, ts)

	ids, _, ts, _, err = ParseM3FinResponse([]byte(`{ "actions": [] }`))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, []int{}, ids)
	assert.Equal(t, []string{}, ts)
}

// capture db takes no argument: one run monitors one database, so the target is
// the agent's own configuration.
func TestParseJsonRespCaptureDB(t *testing.T) {
	for _, tt := range []struct {
		name     string
		resp     string
		wantPIDs []int
		wantDB   bool
	}{
		{"the database action on its own", `{"actions":["capture db"]}`, []int{}, true},
		{"beside a PID capture", `{"actions":["capture 2116","capture db"]}`, []int{2116}, true},
		{"no action", `{"actions":[]}`, []int{}, false},
		{"a PID capture alone", `{"actions":["capture 2116"]}`, []int{2116}, false},

		// "db" is not a PID, and never was: the old parser dropped it silently.
		{"an unparsable argument", `{"actions":["capture nope"]}`, []int{}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pids, _, _, captureDB, err := ParseM3FinResponse([]byte(tt.resp))
			if err != nil {
				t.Fatal(err)
			}

			assert.Equal(t, tt.wantPIDs, pids)
			assert.Equal(t, tt.wantDB, captureDB)
		})
	}
}
