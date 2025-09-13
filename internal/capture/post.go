package capture

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"yc-agent/internal/config"
)

func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return net.IPv4(127, 0, 0, 1)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

func PostData(endpoint, dt string, file *os.File) (msg string, ok bool) {
	return PostCustomData(endpoint, "dt="+dt, file)
}

func PostDataWithTimeout(endpoint, dt string, file *os.File, timeout time.Duration) (msg string, ok bool) {
	return PostCustomDataWithTimeout(endpoint, "dt="+dt, file, timeout)
}

func PositionZero(file *os.File) (err error) {
	_, err = file.Seek(0, io.SeekStart)
	return
}

func PositionLast5000Lines(file *os.File) (err error) {
	return PositionLastLines(file, 5000)
}

func PositionLastLines(file *os.File, n uint) (err error) {
	var cursor int64 = 0
	stat, err := file.Stat()
	if err != nil {
		return
	}
	size := stat.Size()
	char := make([]byte, 1)
	lines := n
	for {
		cursor -= 1
		_, err = file.Seek(cursor, io.SeekEnd)
		if err != nil {
			return
		}
		_, err = file.Read(char)
		if err != nil {
			return
		}
		switch char[0] {
		case '\r':
		case '\n':
			lines--
		}
		if lines == 0 {
			return
		}
		if cursor == -size {
			_, err = file.Seek(0, io.SeekStart)
			return
		}
	}
}

func PostCustomData(endpoint, params string, file *os.File) (msg string, ok bool) {
	return PostCustomDataWithPositionFunc(endpoint, params, file, PositionZero)
}

func PostCustomDataWithTimeout(endpoint, params string, file *os.File, timeout time.Duration) (msg string, ok bool) {
	return PostCustomDataWithPositionFuncWithTimeout(endpoint, params, file, PositionZero, timeout)
}

func PostCustomDataWithPositionFunc(endpoint, params string, file *os.File, position func(file *os.File) error) (msg string, ok bool) {
	return PostCustomDataWithPositionFuncWithTimeout(endpoint, params, file, position, config.GlobalConfig.HttpClientTimeout.Duration())
}

func PostCustomDataWithPositionFuncWithTimeout(endpoint, params string, file *os.File, position func(file *os.File) error, timeout time.Duration) (msg string, ok bool) {
	if config.GlobalConfig.OnlyCapture {
		msg = "in only capture mode"
		return
	}
	if file == nil {
		msg = "file is not captured"
		return
	}
	stat, err := file.Stat()
	if err != nil {
		msg = fmt.Sprintf("file stat err %s", err.Error())
		return
	}
	fileName := stat.Name()
	if stat.Size() < 1 {
		msg = fmt.Sprintf("skipped empty file %s", fileName)
		return
	}

	url := fmt.Sprintf("%s&%s", endpoint, params)
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !config.GlobalConfig.VerifySSL,
	}
	path := config.GlobalConfig.CACertPath
	if len(path) > 0 {
		pool := x509.NewCertPool()
		ca, err := os.ReadFile(path)
		if err != nil {
			msg = err.Error()
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
	err = position(file)
	if err != nil {
		msg = fmt.Sprintf("PostData position err %s", err.Error())
		return
	}
	req, err := http.NewRequest("POST", url, file)
	if err != nil {
		msg = fmt.Sprintf("PostData new req err %s", err.Error())
		return
	}
	req.Header.Set("Content-Type", "text")
	req.Header.Set("ApiKey", config.GlobalConfig.ApiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		msg = fmt.Sprintf("PostData post err %s", err.Error())
		return
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		msg = fmt.Sprintf("PostData get resp err %s", err.Error())
		return
	}
	msg = fmt.Sprintf("%s\nstatus code %d\n%s", url, resp.StatusCode, body)

	if resp.StatusCode == http.StatusOK {
		ok = true
	}
	return
}

func GetData(endpoint string) (msg string, ok bool) {
	if config.GlobalConfig.OnlyCapture {
		msg = "in only capture mode"
		return
	}
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !config.GlobalConfig.VerifySSL,
	}
	path := config.GlobalConfig.CACertPath
	if len(path) > 0 {
		pool := x509.NewCertPool()
		ca, err := os.ReadFile(path)
		if err != nil {
			msg = err.Error()
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   config.GlobalConfig.HttpClientTimeout.Duration(),
	}
	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		msg = fmt.Sprintf("GetData new req err %s", err.Error())
		return
	}
	req.Header.Set("ApiKey", config.GlobalConfig.ApiKey)
	resp, err := httpClient.Do(req)
	if err != nil {
		msg = fmt.Sprintf("GetData get err %s", err.Error())
		return
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		msg = fmt.Sprintf("GetData get resp err %s", err.Error())
		return
	}
	msg = fmt.Sprintf("%s\nstatus code %d\n%s", endpoint, resp.StatusCode, body)

	if resp.StatusCode == http.StatusOK {
		ok = true
	}
	return
}
