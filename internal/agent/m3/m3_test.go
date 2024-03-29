package m3

import (
	"strconv"
	"strings"
	"testing"
)

// https://tier1app.atlassian.net/browse/GCEA-1780
func TestProcessResp(t *testing.T) {
	err := processM3FinResponse([]byte(`{"actions":["capture 1"], "tags":["tag1", "tag2"]}`), map[int]string{1: "abc"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestM3FinPids(t *testing.T) {
	var a = func(pids []int) string {
		if len(pids) <= 0 {
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
