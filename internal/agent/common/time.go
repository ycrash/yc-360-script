package common

import (
	"time"

	"github.com/thlib/go-timezone-local/tzlocal"
)

// GetAgentCurrentTime returns current time and the associated timezone in IANA format.
// This can be used for communication with yc-server's endpoints which require timestamp and timezone.
func GetAgentCurrentTime() (time.Time, string) {
	tz, err := tzlocal.RuntimeTZ()
	if err != nil {
		// if err, fallback to UTC
		return time.Now().UTC(), "UTC"
	}

	return time.Now(), tz
}
