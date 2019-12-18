package shell

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
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
	err := file.Sync()
	if err != nil {
		panic(fmt.Errorf("file sync err %w", err))
	}
	stat, err := file.Stat()
	if err != nil {
		panic(fmt.Errorf("file stat err %w", err))
	}
	fileName := stat.Name()
	if stat.Size() < 1 {
		msg = fmt.Sprintf("skipped empty file %s", fileName)
		return
	}

	url := fmt.Sprintf("%s&dt=%s", endpoint, dt)
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	_, err = file.Seek(0, 0)
	if err != nil {
		panic(fmt.Errorf("file %s seek err %w", fileName, err))
	}
	resp, err := httpClient.Post(url, "Content-Type:text", file)
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
