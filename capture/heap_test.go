package capture

import (
	"fmt"
	"os"
	"testing"
	"time"

	"shell"
)

const (
	api  = "tier1app@12312-12233-1442134-112"
	host = "https://test.gceasy.io"
)

var (
	endpoint     string
	heapEndpoint string
	javaHome     = "/usr/lib/jvm/java-11-openjdk-amd64"
)

func init() {
	if _, err := os.Stat("testdata"); os.IsNotExist(err) {
		err = os.Mkdir("testdata", 0777)
		if err != nil {
			panic(err)
		}
	}
	err := os.Chdir("testdata")
	if err != nil {
		panic(err)
	}
	jh := os.Getenv("JAVA_HOME")
	if len(jh) > 0 {
		javaHome = jh
	}
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", shell.GetOutboundIP().String(), timestamp)
	heapEndpoint = fmt.Sprintf("%s/ycrash-receiver-heap?apiKey=%s&%s", host, api, parameters)
	endpoint = fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", host, api, parameters)
}

func TestHeapDump(t *testing.T) {
	t.Run("with-pid", testHeapDump(""))
	t.Run("with-invalid-pid", testHeapDumpWithInvalidPid)
	t.Run("with-hdPath", testHeapDump("threaddump.out"))
	t.Run("with-invalid-hdPath", testHeapDump("threaddump-non.out"))
}

func testHeapDump(hdPath string) func(t *testing.T) {
	return func(t *testing.T) {
		noGC, err := shell.CommandStartInBackground(shell.Command{"java", "MyClass"})
		if err != nil {
			t.Fatal(err)
		}
		defer noGC.KillAndWait()
		capHeapDump := NewHeapDump(javaHome, noGC.Process.Pid, hdPath)
		capHeapDump.SetEndpoint(heapEndpoint)
		r, err := capHeapDump.Run()
		if err != nil {
			t.Fatal(err)
		}
		if !r.Ok {
			t.Fatal(r)
		} else {
			t.Log(r)
		}
	}
}

func testHeapDumpWithInvalidPid(t *testing.T) {
	var err error
	capHeapDump := NewHeapDump(javaHome, 65535, "")
	capHeapDump.SetEndpoint(heapEndpoint)
	r, err := capHeapDump.Run()
	if err == nil || r.Ok {
		t.Fatal(r)
	}
}
