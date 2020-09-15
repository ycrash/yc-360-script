package shell

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
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

func PostCustomData(endpoint, params string, file *os.File) (msg string, ok bool) {
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
	_, err = file.Seek(0, 0)
	if err != nil {
		panic(fmt.Errorf("file %s seek err %w", fileName, err))
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
