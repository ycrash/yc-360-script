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

// PositionLastLines moves the file offset so that the next read returns the last n lines.
//
// Lines are separated by '\n'. If the file does not end with '\n', the final partial
// line counts as a line. If the file ends with '\n', that newline is treated as the
// terminator of the last line (not as an extra empty line).
//
// If the file has fewer than n lines, the offset is set to the beginning of the file.
// If n == 0, the offset is set to end-of-file.
// The function returns an error if any I/O operation fails.
func PositionLastLines(file *os.File, n uint) (err error) {
	// n == 0: "last 0 lines" -> position at EOF
	if n == 0 {
		_, err := file.Seek(0, io.SeekEnd)
		if err != nil {
			return fmt.Errorf("seek to end: %w", err)
		}
		return nil
	}

	// Determine file size
	fileSize, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return fmt.Errorf("seek to end: %w", err)
	}
	if fileSize == 0 {
		// Empty file: position at start
		_, err := file.Seek(0, io.SeekStart)
		if err != nil {
			return fmt.Errorf("seek to start: %w", err)
		}
		return nil
	}

	// Check whether the file ends with a newline
	var lastByte [1]byte
	if _, err := file.ReadAt(lastByte[:], fileSize-1); err != nil {
		return fmt.Errorf("read last byte: %w", err)
	}
	endsWithNewline := lastByte[0] == '\n'

	// To get the last n lines, we count newlines from the end:
	// - If file ends with '\n': we want to find the (n+1)th newline from EOF,
	//   then start reading after it. The trailing '\n' is the line terminator
	//   for the last line, not an extra empty line.
	// - If file doesn't end with '\n': we want the nth newline from EOF.
	//
	// Example: "a\nb\nc\n" with n=1 should give "c\n"
	//   - We skip the trailing '\n' (line terminator)
	//   - Find 1 more '\n' before it (the one after 'b')
	//   - Start after that '\n' → "c\n"
	//
	// Example: "a\nb\nc" with n=1 should give "c"
	//   - No trailing '\n'
	//   - Find 1 '\n' from the end (the one after 'b')
	//   - Start after that '\n' → "c"
	targetNewlines := int(n)
	if endsWithNewline {
		targetNewlines = int(n) + 1
	}

	const bufferSize = 4096
	buf := make([]byte, bufferSize)

	newlinesSeen := 0
	position := fileSize // position is the start offset of the next block to read

	for position > 0 && newlinesSeen < targetNewlines {
		// Determine how much to read
		blockSize := bufferSize
		if int64(blockSize) > position {
			blockSize = int(position)
		}

		// Move to the previous block
		position -= int64(blockSize)

		// Read block at fixed position without changing the current file offset
		nRead, err := file.ReadAt(buf[:blockSize], position)
		if err != nil && err != io.EOF {
			return fmt.Errorf("read chunk at %d: %w", position, err)
		}
		chunk := buf[:nRead]

		// Scan this chunk backwards for newline characters
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] == '\n' {
				newlinesSeen++
				if newlinesSeen == targetNewlines {
					// Start just after this newline
					start := position + int64(i+1)
					_, err := file.Seek(start, io.SeekStart)
					if err != nil {
						return fmt.Errorf("seek to %d: %w", start, err)
					}
					return nil
				}
			}
		}
	}

	// Not enough newline characters: the whole file is within the last n lines
	_, err = file.Seek(0, io.SeekStart)
	if err != nil {
		return fmt.Errorf("seek to start: %w", err)
	}
	return nil
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
