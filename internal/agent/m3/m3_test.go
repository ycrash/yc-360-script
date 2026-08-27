package m3

import (
	"net/url"
	"strconv"
	"strings"
	"testing"

	"yc-agent/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// The agent cannot see other runners, so two of them polling one database is only
// visible to the server - from the target each fin call names.
func TestGetM3FinEndpointNamesTheDatabaseTarget(t *testing.T) {
	original := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = original })

	t.Run("no postgres block leaves the query untouched", func(t *testing.T) {
		config.GlobalConfig = config.Config{}

		endpoint := GetM3FinEndpoint("2026-08-27T09-12-00", "UTC", nil)

		assert.NotContains(t, endpoint, "target_host")
		assert.NotContains(t, endpoint, "target_database")
	})

	t.Run("a configured target is named", func(t *testing.T) {
		config.GlobalConfig = config.Config{Options: config.Options{
			Postgres: &config.Postgres{Host: "db-prod-01", Port: 5432, Database: "orders"},
		}}

		endpoint := GetM3FinEndpoint("2026-08-27T09-12-00", "UTC", nil)

		assert.Contains(t, endpoint, "&target_host=db-prod-01")
		assert.Contains(t, endpoint, "&target_port=5432")
		assert.Contains(t, endpoint, "&target_database=orders")
	})

	// A socket directory and an IPv6 address both carry characters that would end
	// the value early, taking the parameters after it with them.
	t.Run("a socket path is escaped", func(t *testing.T) {
		config.GlobalConfig = config.Config{Options: config.Options{
			Postgres: &config.Postgres{Host: "/var/run/postgresql", Port: 5432, Database: "a b&c"},
		}}

		endpoint := GetM3FinEndpoint("2026-08-27T09-12-00", "UTC", nil)

		assert.Contains(t, endpoint, "&target_host=%2Fvar%2Frun%2Fpostgresql")
		assert.Contains(t, endpoint, "&target_database=a+b%26c")

		parsed, err := url.Parse(endpoint)
		require.NoError(t, err)

		assert.Equal(t, "/var/run/postgresql", parsed.Query().Get("target_host"))
		assert.Equal(t, "a b&c", parsed.Query().Get("target_database"))
	})
}
