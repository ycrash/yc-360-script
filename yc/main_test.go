package main

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"shell"
)

const (
	api  = "tier1app@12312-12233-1442134-112"
	host = "https://test.gceasy.io"
)

func TestFindGCLog(t *testing.T) {
	noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()

	xlog, err := shell.CommandStartInBackground(shell.Command{"java", "-Xlog:gc=trace:file=gctrace.txt:uptimemillis,pid:filecount=5,filesize=1024", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xlog.KillAndWait()

	xlog2, err := shell.CommandStartInBackground(shell.Command{"java", "-Xlog:gc:gctrace.log", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xlog2.KillAndWait()

	xloggc, err := shell.CommandStartInBackground(shell.Command{"java", "-Xloggc:garbage-collection.log", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xloggc.KillAndWait()

	f, err := getGCLogFile(noGC.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if len(f) > 0 {
		t.Fatal("gc log file should be empty")
	}

	f, err = getGCLogFile(xlog.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.txt" {
		t.Fatal("gc log file should be gctrace.txt")
	}

	f, err = getGCLogFile(xlog2.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.log" {
		t.Fatal("gc log file should be gctrace.log")
	}

	f, err = getGCLogFile(xloggc.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "garbage-collection.log" {
		t.Fatal("gc log file should be garbage-collection.log")
	}

}

func TestPostData(t *testing.T) {
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", host, api, parameters)

	t.Run("requestFin", func(t *testing.T) {
		finEp := fmt.Sprintf("%s/yc-fin?apiKey=%s&%s", host, api, parameters)
		requestFin(finEp)
	})

	vmstat, err := os.Open("testdata/vmstat.out")
	if err != nil {
		return
	}
	defer vmstat.Close()
	ps, err := os.Open("testdata/ps.out")
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	top, err := os.Open("testdata/top.out")
	if err != nil {
		t.Fatal(err)
	}
	defer top.Close()
	df, err := os.Open("testdata/disk.out")
	if err != nil {
		t.Fatal(err)
	}
	defer df.Close()
	netstat, err := os.Open("testdata/netstat.out")
	if err != nil {
		t.Fatal(err)
	}
	defer netstat.Close()
	gc, err := os.Open("testdata/gc.log")
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	td, err := os.Open("testdata/threaddump.out")
	if err != nil {
		t.Fatal(err)
	}
	defer td.Close()

	msg, ok := postData(endpoint, "top", top)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "df", df)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "ns", netstat)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "ps", ps)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "vmstat", vmstat)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "gc", gc)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = postData(endpoint, "td", td)
	if !ok {
		t.Fatal("post data failed", msg)
	}
}

func init() {
	err := os.Chdir("testdata")
	if err != nil {
		panic(err)
	}
}

func TestWriteMetaInfo(t *testing.T) {
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", host, api, parameters)
	msg, ok, err := writeMetaInfo(11111, "test", endpoint)
	if err != nil || !ok {
		t.Fatal(err, msg)
	}
	t.Log(msg, ok)
}

func TestProcessLogFile(t *testing.T) {
	fatalIf := func(err error) {
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	fatalIf(os.Remove("gc-rotation-logs/0-current/1/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/0-current/2/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/0-current/3/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/1-current/gc.log"))
	fatalIf(os.Remove("gc-rotation-logs/2-current/gc.log"))
	test := func(t *testing.T, dir string, fname string, out string) {
		if len(out) < 1 {
			out = fname
		}
		gc, err := processGCLogFile(filepath.Join(dir, fname), filepath.Join(dir, out))
		if err != nil {
			t.Fatal(err)
		}
		gc.Seek(0, 0)
		all, err := ioutil.ReadAll(gc)
		if err != nil {
			t.Fatal(err)
		}
		s := string(all)
		if s != fmt.Sprintf("test\ntest") {
			t.Fatal(s)
		}
	}
	t.Run("0-current-1", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/1"
		test(t, dir, "gc.log", "")
	})
	t.Run("0-current-2", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/2"
		test(t, dir, "gc.log", "")
	})
	t.Run("0-current-3", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/3"
		test(t, dir, "gc.log", "")
	})
	t.Run("1-current", func(t *testing.T) {
		dir := "gc-rotation-logs/1-current"
		test(t, dir, "gc.log", "")
	})
	t.Run("2-current", func(t *testing.T) {
		dir := "gc-rotation-logs/2-current"
		test(t, dir, "gc.log", "")
	})

	fatalIf(os.Remove("gc-rotation-logs/0-current/1/gc.log"))
	t.Run("gcPath-exists", func(t *testing.T) {
		dir := "gc-rotation-logs/0-current/1"
		test(t, dir, "gc.log.0.current", "gc.log")
	})
	t.Run("gcPath-not-exists", func(t *testing.T) {
		_, err := processGCLogFile("gc-rotation-logs/0-current/1/gc.log.current",
			"gc-rotation-logs/0-current/1/gc.log")
		if err != nil && errors.Is(err, os.ErrNotExist) && strings.Contains(err.Error(), "can not find the current log file,") {
		} else {
			t.Fatal(err)
		}
	})
}

func TestCaptureCmd(t *testing.T) {
	_, err := runCaptureCmd(123, "echo $pid")
	if err != nil {
		t.Fatal(err)
	}
}

// https://tier1app.atlassian.net/browse/GCEA-1780
func TestProcessResp(t *testing.T) {
	err := processResp([]byte(`{"actions":["capture 1"]}`))
	if err != nil {
		t.Fatal(err)
	}
}
