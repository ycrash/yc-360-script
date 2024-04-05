package common

import (
	"net"
	"testing"
	"time"
)

func TestSleep4Attendance(t *testing.T) {
	utc := time.Now().UTC()
	target := utc.Truncate(24 * time.Hour)
	target = target.AddDate(0, 0, 1)
	t.Logf("now is %s, will do attendance task at %s, after %s", utc.Format("2006/01/02 15:04:05"), target.Format("2006/01/02 15:04:05"), target.Sub(utc))

	t.Run("calDuration4Attendance", func(t *testing.T) {
		time, err := time.ParseInLocation("2006/01/02 15:04:05", "2020/09/16 00:04:31", time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		time, _ = calDuration4Attendance(time)
		format := time.Format("2006/01/02 15:04:05")
		if format != "2020/09/17 00:00:00" {
			t.Fatal(format)
		}
	})
}

func TestSleep4Distribution(t *testing.T) {
	d := calDuration4Distribution(net.ParseIP("129.45.12.123"))
	var b byte
	// adding two 255 here is because the IP address is in 16-byte(ipv6) form with two 0xff prefix
	b += 255
	b += 255
	b += 129
	b += 45
	b += 12
	b += 123
	bd := time.Duration(b%10) * time.Minute
	t.Logf("%s %d", d, bd)
	if d != bd {
		t.Fail()
	}
}
