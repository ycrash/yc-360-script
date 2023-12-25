package shell

import (
	"encoding/json"
	"strconv"
	"strings"
)

type M3FinResponse struct {
	Actions    []string
	Tags       []string
	Timestamp  string
	Timestamps []string
}

func ParseM3FinResponse(resp []byte) (pids []int, tags []string, timestamps []string, err error) {
	// Init empty slice instead of []int(nil)
	pids = []int{}
	tags = []string{}
	timestamps = []string{}

	r := &M3FinResponse{}
	err = json.Unmarshal(resp, r)
	if err != nil {
		return
	}

	tags = r.Tags
	if len(r.Timestamps) > 0 {
		// If the new "timestamps" field is present
		timestamps = r.Timestamps
	} else if r.Timestamp != "" {
		// If the new "timestamps" is not present,
		// Use the legacy "timestamp" field
		timestamps = append(timestamps, r.Timestamp)
	}

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
