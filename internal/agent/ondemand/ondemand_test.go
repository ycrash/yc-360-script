package ondemand

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	api  = "tier1app@12312-12233-1442134-112"
	host = "https://test.gceasy.io"
)

func TestFindGCLog(t *testing.T) {
	// Skip: Test requires Java environment with compiled MyClass.java to run background Java processes.
	// This is an integration test that verifies GC log detection from running Java processes.
	t.Skip("Skipping: test requires Java environment with compiled test class")

	noGC, err := executils.CommandStartInBackground(executils.Command{"java", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer noGC.KillAndWait()

	xlog, err := executils.CommandStartInBackground(executils.Command{"java", "-Xlog:gc=trace:file=gctrace.txt:uptimemillis,pid:filecount=5,filesize=1024", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xlog.KillAndWait()

	xlog2, err := executils.CommandStartInBackground(executils.Command{"java", "-Xlog:gc:gctrace.log", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xlog2.KillAndWait()

	xloggc, err := executils.CommandStartInBackground(executils.Command{"java", "-Xloggc:garbage-collection.log", "MyClass"})
	if err != nil {
		t.Fatal(err)
	}
	defer xloggc.KillAndWait()

	f, err := GetGCLogFile(noGC.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if len(f) > 0 {
		t.Fatal("gc log file should be empty")
	}

	f, err = GetGCLogFile(xlog.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.txt" {
		t.Fatal("gc log file should be gctrace.txt")
	}

	f, err = GetGCLogFile(xlog2.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "gctrace.log" {
		t.Fatal("gc log file should be gctrace.log")
	}

	f, err = GetGCLogFile(xloggc.GetPid())
	if err != nil {
		t.Fatal(err)
	}
	t.Log(f)
	if f != "garbage-collection.log" {
		t.Fatal("gc log file should be garbage-collection.log")
	}
}

func TestPostData(t *testing.T) {
	// Track received requests for assertions
	var mu sync.Mutex
	receivedRequests := make(map[string]struct {
		method   string
		bodySize int
	})

	// Create mock HTTP server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read body to get size
		body, _ := io.ReadAll(r.Body)
		defer r.Body.Close()

		// Extract dt parameter
		dt := r.URL.Query().Get("dt")

		mu.Lock()
		receivedRequests[dt] = struct {
			method   string
			bodySize int
		}{
			method:   r.Method,
			bodySize: len(body),
		}
		mu.Unlock()

		w.WriteHeader(http.StatusOK)
		w.Write([]byte("success"))
	}))
	defer server.Close()

	// Build endpoint using mock server URL
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", capture.GetOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", server.URL, api, parameters)

	// Test data files - path relative to ondemand package
	testFiles := []struct {
		path   string
		dtType string
	}{
		{"../testdata/top.out", "top"},
		{"../testdata/disk.out", "df"},
		{"../testdata/netstat.out", "ns"},
		{"../testdata/ps.out", "ps"},
		{"../testdata/vmstat.out", "vmstat"},
		{"../testdata/gc.log", "gc"},
		{"../testdata/threaddump.out", "td"},
	}

	for _, tf := range testFiles {
		t.Run(tf.dtType, func(t *testing.T) {
			file, err := os.Open(tf.path)
			require.NoError(t, err, "Failed to open test file: %s", tf.path)
			defer file.Close()

			msg, ok := capture.PostData(endpoint, tf.dtType, file)
			assert.True(t, ok, "PostData failed for %s: %s", tf.dtType, msg)
		})
	}

	// Verify all requests were received correctly
	mu.Lock()
	defer mu.Unlock()

	for _, tf := range testFiles {
		req, exists := receivedRequests[tf.dtType]
		assert.True(t, exists, "Request for dt=%s was not received", tf.dtType)
		if exists {
			assert.Equal(t, "POST", req.method, "Expected POST method for dt=%s", tf.dtType)
			assert.Greater(t, req.bodySize, 0, "Expected non-empty body for dt=%s", tf.dtType)
		}
	}
}

func TestWriteMetaInfo(t *testing.T) {
	// Skip: Test makes external HTTP calls to test.gceasy.io which is flaky in CI.
	// This is an integration test that should be run manually or in a dedicated integration test suite.
	t.Skip("Skipping: test makes external HTTP calls to test.gceasy.io")

	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", capture.GetOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", host, api, parameters)
	msg, ok, err := writeMetaInfo(11111, "test", endpoint, "tag1")
	if err != nil || !ok {
		t.Fatal(err, msg)
	}
	t.Log(msg, ok)
}

func TestAllPossibleGCPath(t *testing.T) {
	var cmdLine = "ProcessId java  -Xlog:gc*=info,gc+heap=debug,gc+ref*=debug,gc+ergo*=trace,gc+age*=trace:file=/tmp/gc.log:utctime,pid,level,tags:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	expected := "/tmp/gc.log"
	logFile := ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 1: -Xlog:gc:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 2: -Xlog:gc:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 3: -Xlog:gc+heap=info:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=info:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 4: -Xlog:gc+heap=info:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+ref=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 5: -Xlog:gc+heap=info:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+ref=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 6: -Xlog:gc+age=trace:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+age=trace:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 7: -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=debug:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 8: -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 9: -Xlog:gc*:file=/tmp/gc.log:time,tags
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:time,tags -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 10: -Xlog:gc*:file=/tmp/gc.log:utctime
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 11: -Xlog:gc*:file=/tmp/gc.log:uptime,level
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:uptime,level -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 12: -Xlog:gc*:file=/tmp/gc.log:utctime,pid
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime,pid -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 13: -Xlog:gc*:file=/tmp/gc.log:utctime,pid,level,tags
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime,pid,level,tags -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 14: -Xlog:gc*:file=/tmp/gc.log:uptime,level:filecount=2,filesize=100M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:uptime,level:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 15: -Xlog:gc*=debug:file=/tmp/gc.log:time,pid,level:filecount=10,filesize=20M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=debug:file=/tmp/gc.log:time,pid,level:filecount=10,filesize=20M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 16: -Xlog:gc*=trace:file=/tmp/gc.log:uptime,tags:filecount=3,filesize=5M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=trace:file=/tmp/gc.log:uptime,tags:filecount=3,filesize=5M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 17: -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log:utctime,level,filecount=4,filesize=50M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log:utctime,level,filecount=4,filesize=50M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 18: -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 19: -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=trace,gc+age*=trace:file=/tmp/gc.log:utctime,pid,level,tags:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)

	// case 20: -Xlog:gc*=info:file=/tmp/gc.log:noforcetty
	cmdLine = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info:file=/tmp/gc.log:noforcetty -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"
	logFile = ExtractGCLogPathFromCmdline(cmdLine)
	assert.Equal(t, expected, logFile)
}
