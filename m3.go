package shell

import (
	"encoding/json"
	"strconv"
	"strings"
)

// M3Resp is a deprecated schema with Timestamp as string.
// For the transition, we support both, and will retire this old schema in later version.
type M3Resp struct {
	Actions   []string
	Tags      []string
	Timestamp string
}

// M3Resp2 is the new schema with Timestamp as an array.
// For the transition, we support both, and will retire the old schema in later version.
type M3Resp2 struct {
	Actions   []string
	Tags      []string
	Timestamp []string
}

func ParseJsonResp(resp []byte) (pids []int, tags []string, timestamps []string, err error) {
	// Init empty slice instead of []int(nil)
	pids = []int{}
	tags = []string{}
	timestamps = []string{}
	var actions []string

	r := &M3Resp{}
	err = json.Unmarshal(resp, r)
	if err != nil {
		r2 := &M3Resp2{}
		err = json.Unmarshal(resp, r2)
		if err != nil {
			return
		} else {
			// Successfully unmarshal new response schema
			tags = r2.Tags
			timestamps = r2.Timestamp
			actions = r2.Actions
		}
	} else {
		// Successfully unmarshal legacy response schema
		tags = r.Tags
		actions = r.Actions
		if r.Timestamp != "" {
			timestamps = append(timestamps, r.Timestamp)
		}
	}

	for _, s := range actions {
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
