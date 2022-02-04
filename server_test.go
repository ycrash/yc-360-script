package shell

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"shell/config"
	"testing"
)

func TestServer(t *testing.T) {
	s, err := NewServer("localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.ProcessPids = func(pids []int, pid2Name map[int]string) (rUrls []string, err error) {
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

	go func() {
		defer s.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "capture 12321", "capture 2341", "capture findmydeviced"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr().String()), "text", buf)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			if string(all) != `{"Code":0,"Msg":""}` {
				t.Fatal(string(all), all)
			}
		}
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	}
}

func TestServerCmdActions(t *testing.T) {
	s, err := NewServer("localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.ProcessPids = func(pids []int, pid2Name map[int]string) (rUrls []string, err error) {
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

	go func() {
		defer s.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "date", "capture 2341", "echo $pid"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr().String()), "text", buf)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			if !bytes.HasPrefix(all, []byte(`{"Code":0`)) {
				t.Fatalf("%s, %x", string(all), all)
			}
		}
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	}
}

func TestServerForward(t *testing.T) {
	s, err := NewServer("localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.ProcessPids = func(pids []int, pid2Name map[int]string) (rUrls []string, err error) {
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

	rs, err := NewServer("localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	rs.ProcessPids = func(pids []int, pid2Name map[int]string) (rUrls []string, err error) {
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

	go func() {
		defer s.Close()
		defer rs.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "capture 12321", "capture 2341", "capture findmydeviced"] }`)
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/action", s.Addr().String()), buf)
		if err != nil {
			t.Fatal(err)
		}
		req.Close = true
		req.Header.Add("ycrash-forward", fmt.Sprintf("http://%s/action", rs.Addr().String()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			if string(all) != `{"Code":0,"Msg":""}` {
				t.Fatal(string(all), all)
			}
		}
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
	}
}

func TestAttendanceAPI(t *testing.T) {
	s, err := NewServer("localhost", 0)
	if err != nil {
		t.Fatal(err)
	}
	s.ProcessPids = func(pids []int, pid2Name map[int]string) (rUrls []string, err error) {
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

	go func() {
		defer s.Close()
		config.GlobalConfig.Server = "https://test.gceasy.io"
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "attendance"] }`)
		resp, err := http.Post(fmt.Sprintf("http://%s/action", s.Addr().String()), "text", buf)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			if string(all) != `{"Code":0,"Msg":""}` {
				t.Fatal(all)
			}
		}
	}()

	select {
	case err, ok := <-errCh:
		if ok {
			t.Fatal(err)
		}
	}
}
