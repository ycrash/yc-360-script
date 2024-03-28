package utils

import (
	"io"
	"net/http"
	"strings"
	"time"
)

func GetServerTimeZone() string {
	// Make a request to ipinfo.io to get timezone information based on the server's IP address
	resp, err := http.Get("https://ipinfo.io/timezone")

	serverTime := time.Now()
	fallbackZone, _ := serverTime.Zone()

	if err != nil {
		return fallbackZone
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fallbackZone
	}

	timezone := strings.TrimSpace(string(body))
	if timezone == "" {
		return fallbackZone
	}

	return timezone
}
