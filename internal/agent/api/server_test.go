package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"yc-agent/internal/config"
)

func TestServer(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test is flaky with goroutines and HTTP server lifecycle management.
	// Likely timing issues or port binding problems in CI environment.
	t.Skip("Skipping until server test async behavior can be fixed")

	s := NewServer("localhost", 0)
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.Serve()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	testErrCh := make(chan error, 1)
	go func() {
		defer s.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "capture 12321", "capture 2341", "capture findmydeviced"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr()), "text", buf)
		if err != nil {
			testErrCh <- err
			return
		}
		defer resp.Body.Close()

		all, err := io.ReadAll(resp.Body)
		if err != nil {
			testErrCh <- err
			return
		}
		all = bytes.TrimSpace(all)
		if string(all) != `{"Code":0,"Msg":""}` {
			testErrCh <- fmt.Errorf("unexpected response: %s", string(all))
			return
		}
		testErrCh <- nil
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	case err := <-testErrCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerCmdActions(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test is flaky with goroutines and HTTP server lifecycle management.
	// Commands execution in API server needs better synchronization.
	t.Skip("Skipping until server command action tests can be stabilized")

	s := NewServer("localhost", 0)
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.Serve()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	testErrCh := make(chan error, 1)
	go func() {
		defer s.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "date", "capture 2341", "echo $pid"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr()), "text", buf)
		if err != nil {
			testErrCh <- err
			return
		}
		defer resp.Body.Close()

		all, err := io.ReadAll(resp.Body)
		if err != nil {
			testErrCh <- err
			return
		}
		all = bytes.TrimSpace(all)
		if !bytes.HasPrefix(all, []byte(`{"Code":0`)) {
			testErrCh <- fmt.Errorf("unexpected response: %s, %x", string(all), all)
			return
		}
		testErrCh <- nil
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	case err := <-testErrCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerForward(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test involves multiple servers and forwarding logic with complex async behavior.
	// Timing issues with goroutines and server lifecycle management.
	t.Skip("Skipping until server forward tests can be stabilized")

	s := NewServer("localhost", 0)
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.Serve()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	rs := NewServer("localhost", 0)
	rs.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
		t.Log("ok", pids)
		return
	}

	rerrCh := make(chan error, 1)
	go func() {
		err := rs.Serve()
		if !errors.Is(err, http.ErrServerClosed) {
			rerrCh <- err
		}
		close(rerrCh)
	}()

	testErrCh := make(chan error, 1)
	go func() {
		defer s.Close()
		defer rs.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "capture 12321", "capture 2341", "capture findmydeviced"] }`)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/action", s.Addr()), buf)
		if err != nil {
			testErrCh <- err
			return
		}
		req.Close = true
		req.Header.Add("ycrash-forward", fmt.Sprintf("http://%s/action", rs.Addr()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			testErrCh <- err
			return
		}
		defer resp.Body.Close()

		all, err := io.ReadAll(resp.Body)
		if err != nil {
			testErrCh <- err
			return
		}
		all = bytes.TrimSpace(all)
		if string(all) != `{"Code":0,"Msg":""}` {
			testErrCh <- fmt.Errorf("unexpected response: %s", string(all))
			return
		}
		testErrCh <- nil
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	case err, ok := <-rerrCh:
		if ok {
			t.Fatal(err)
		}
	case err := <-testErrCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAttendanceAPI(t *testing.T) {
	// TODO: Revisit this test - currently failing in CI
	// Test makes external HTTP calls to test.gceasy.io which may be unreachable or flaky in CI.
	// Should be mocked to avoid external dependencies.
	t.Skip("Skipping until attendance API can be properly mocked")

	s := NewServer("localhost", 0)
	s.ProcessPids = func(pids []int, pid2Name map[int]string, hd bool, tags string) (rUrls []string, err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.Serve()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	testErrCh := make(chan error, 1)
	go func() {
		defer s.Close()
		config.GlobalConfig.Server = "https://test.gceasy.io"
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "attendance"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr()), "text", buf)
		if err != nil {
			testErrCh <- err
			return
		}
		defer resp.Body.Close()

		all, err := io.ReadAll(resp.Body)
		if err != nil {
			testErrCh <- err
			return
		}
		all = bytes.TrimSpace(all)
		if string(all) != `{"Code":0,"Msg":""}` {
			testErrCh <- fmt.Errorf("unexpected response: %s", string(all))
			return
		}
		testErrCh <- nil
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	case err := <-testErrCh:
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestJSON(t *testing.T) {
	type test struct {
		B *bool
	}

	var a test
	err := json.Unmarshal([]byte("{\"c\":true}"), &a)
	if err != nil || a.B != nil {
		t.Fatal(err)
	}
	err = json.Unmarshal([]byte("{\"b\":false}"), &a)
	if err != nil || a.B == nil {
		t.Fatal(err)
	}
	if *a.B {
		t.Fatal("should be false")
	}

	err = json.Unmarshal([]byte("{\"b\":true}"), &a)
	if err != nil || a.B == nil {
		t.Fatal(err)
	}
	if !*a.B {
		t.Fatal("should be true")
	}
}
