package main

import (
	"fmt"
	"io/ioutil"
	"os"
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

	f, err := getGCLogFile(noGC.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if len(f) > 0 {
		t.Fatal("gc log file should be empty")
	}

	f, err = getGCLogFile(xlog.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.txt" {
		t.Fatal("gc log file should be gctrace.txt")
	}

	f, err = getGCLogFile(xlog2.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.log" {
		t.Fatal("gc log file should be gctrace.log")
	}

	f, err = getGCLogFile(xloggc.Process.Pid)
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
	logger = Logger{writer: os.Stdout}
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
	err := os.Chdir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	gc, err := processGCLogFile("ogc.log", "tgc.log")
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
