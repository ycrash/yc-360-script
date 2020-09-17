package shell

import (
	"bytes"
	"errors"
	"io/ioutil"
	"net/http"
	"testing"

	"shell/config"
)

func TestServer(t *testing.T) {
	s := NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
	s.ProcessPids = func(pids []int) (err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.ListenAndServe()
		if !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	go func() {
		defer s.Close()
		config.GlobalConfig.ApiKey = "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc"
		buf := bytes.NewBufferString(`{"key": "buggycompany@e094aasdsa-c3eb-4c9a-8254-f0dd107245cc", "actions":[ "capture 12321", "capture 2341", "capture findmydeviced"] }`)
		resp, err := http.Post("http://localhost:8085/action", "text", buf)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			t.Logf("%s", all)
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

func TestAttendanceAPI(t *testing.T) {
	s := NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
	s.ProcessPids = func(pids []int) (err error) {
		t.Log(pids)
		return
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.ListenAndServe()
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
		resp, err := http.Post("http://localhost:8085/action", "text", buf)
		if err != nil {
			t.Fatal(err)
		}

		if resp.Body != nil {
			all, err := ioutil.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			all = bytes.TrimSpace(all)
			t.Logf("%s", all)
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
