package utils

import (
	"fmt"
	"net"
	"time"

	"shell/internal/config"
	"shell/internal/logger"
)

func sleep4Attendance() {
	utc := time.Now().UTC()
	target, sub := calDuration4Attendance(utc)
	logger.Log("now is %s, will do attendance task at %s, after %s", utc.Format("2006/01/02 15:04:05"), target.Format("2006/01/02 15:04:05"), sub)
	time.Sleep(sub)
}

func calDuration4Attendance(utc time.Time) (time.Time, time.Duration) {
	target := utc.Truncate(24 * time.Hour)
	target = target.AddDate(0, 0, 1)
	sub := target.Sub(utc)
	return target, sub
}

func sleep4Distribution() {
	d := calDuration4Distribution(GetOutboundIP())
	logger.Log("sleep4Distribution %s", d)
	if d <= 0 {
		return
	}
	time.Sleep(d)
}

func calDuration4Distribution(ip net.IP) time.Duration {
	bs := []byte(ip)
	var sum byte
	for _, b := range bs {
		sum += b
	}
	m := sum % 10
	d := time.Duration(m) * time.Minute
	return d
}

func AttendWithType(typ string) (string, bool) {
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", GetOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/yc-attendance?type=%s&%s",
		config.GlobalConfig.Server, typ, parameters)
	if config.GlobalConfig.M3 {
		endpoint += "&m3=true"
	}
	return GetData(endpoint)
}

func Attend() (string, bool) {
	sleep4Attendance()

	sleep4Distribution()

	return AttendWithType("daily")
}

func StartupAttend() (string, bool) {
	return AttendWithType("startup")
}
