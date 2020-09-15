package shell

import (
	"fmt"
	"time"

	"shell/config"
	"shell/logger"
)

func Sleep4Attendance() {
	utc := time.Now().UTC()
	target := utc.Truncate(24 * time.Hour)
	target = target.AddDate(0, 0, 1)
	sub := target.Sub(utc)
	logger.Log("now is %s, will do attendance task at %s, after %s", utc.Format("2006/01/02 15:04:05"), target.Format("2006/01/02 15:04:05"), sub)
	time.Sleep(sub)
}

func Attend() (string, bool) {
	Sleep4Attendance()

	ip := GetOutboundIP()
	bs := []byte(ip)
	var sum byte
	for _, b := range bs {
		sum += b
	}
	m := sum % 10
	time.Sleep(time.Duration(m) * time.Minute)

	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", ip.String(), timestamp)
	endpoint := fmt.Sprintf("%s/yc-attendance?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)
	return GetData(endpoint)
}
