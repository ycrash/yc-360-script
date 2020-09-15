package shell

import (
	"testing"
	"time"
)

func TestSleep4Attendance(t *testing.T) {
	utc := time.Now().UTC()
	target := utc.Truncate(24 * time.Hour)
	target = target.AddDate(0, 0, 1)
	t.Logf("now is %s, will do attendance task at %s, after %s", utc.Format("2006/01/02 15:04:05"), target.Format("2006/01/02 15:04:05"), target.Sub(utc))
}

func TestSleep4Distribution(t *testing.T) {
	ip := GetOutboundIP()
	bs := []byte(ip)
	var sum byte
	for _, b := range bs {
		sum += b
	}
	m := sum % 10
	t.Logf("sleep %d min", m)
}
