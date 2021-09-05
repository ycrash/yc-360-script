package shell

import (
	"encoding/json"
	"strconv"
	"strings"
)

type M3Resp struct {
	Actions []string
	Tags    []string
}

func ParseJsonResp(resp []byte) (pids []int, tags []string, err error) {
	r := &M3Resp{}
	err = json.Unmarshal(resp, r)
	if err != nil {
		return
	}
	tags = r.Tags
	for _, s := range r.Actions {
		if strings.HasPrefix(s, "capture ") {
			ss := strings.Split(s, " ")
			if len(ss) == 2 {
				id := ss[1]
				pid, err := strconv.Atoi(id)
				if err != nil {
					continue
				}
				pids = append(pids, pid)
			}
		}
	}
	return
}
