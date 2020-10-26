package shell

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"

	"shell/config"
)

func GetOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

func PostData(endpoint, dt string, file *os.File) (msg string, ok bool) {
	return PostCustomData(endpoint, "dt="+dt, file)
}

func PositionZero(file *os.File) (err error) {
	_, err = file.Seek(0, io.SeekStart)
	return
}

var _5000Lines = 5000

func PositionLast5000Lines(file *os.File) (err error) {
	var cursor int64 = 0
	stat, err := file.Stat()
	if err != nil {
		return
	}
	size := stat.Size()
	char := make([]byte, 1)
	lines := _5000Lines
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
		if cursor != -1 && (char[0] == 10 || char[0] == 13) {
			lines--
			if lines == 0 {
				break
			}
		}
		if cursor == -size {
			_, err = file.Seek(0, io.SeekStart)
			if err != nil {
				return
			}
			break
		}
	}
	return
}

func PostCustomData(endpoint, params string, file *os.File) (msg string, ok bool) {
	return PostCustomDataWithPositionFunc(endpoint, params, file, PositionZero)
}

func PostCustomDataWithPositionFunc(endpoint, params string, file *os.File, position func(file *os.File) error) (msg string, ok bool) {
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
		ca, err := ioutil.ReadFile(path)
		if err != nil {
			msg = err.Error()
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	err = position(file)
	if err != nil {
		msg = fmt.Sprintf("PostData post err %s", err.Error())
		return
	}
	resp, err := httpClient.Post(url, "text", file)
	if err != nil {
		msg = fmt.Sprintf("PostData post err %s", err.Error())
		return
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
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
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !config.GlobalConfig.VerifySSL,
	}
	path := config.GlobalConfig.CACertPath
	if len(path) > 0 {
		pool := x509.NewCertPool()
		ca, err := ioutil.ReadFile(path)
		if err != nil {
			msg = err.Error()
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	resp, err := httpClient.Get(endpoint)
	if err != nil {
		msg = fmt.Sprintf("PostData post err %s", err.Error())
		return
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		msg = fmt.Sprintf("PostData get resp err %s", err.Error())
		return
	}
	msg = fmt.Sprintf("%s\nstatus code %d\n%s", endpoint, resp.StatusCode, body)

	if resp.StatusCode == http.StatusOK {
		ok = true
	}
	return
}
