package ondemand

import (
	"context"
	"os"
	"testing"
	"time"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)

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

			msg, ok := capture.PostData(srv.URLFor(fakeserver.Receiver), tf.dtType, file)
			assert.True(t, ok, "PostData failed for %s: %s", tf.dtType, msg)
		})
	}

	received := map[string]fakeserver.Request{}
	for _, r := range srv.Requests() {
		received[r.DT] = r
	}

	for _, tf := range testFiles {
		req, exists := received[tf.dtType]
		assert.True(t, exists, "Request for dt=%s was not received", tf.dtType)
		if exists {
			assert.Equal(t, "POST", req.Method, "Expected POST method for dt=%s", tf.dtType)
			assert.Greater(t, req.Bytes, int64(0), "Expected non-empty body for dt=%s", tf.dtType)
		}
	}

	assert.Empty(t, srv.Violations())
}

func TestWriteMetaInfo(t *testing.T) {
	javaHome := os.Getenv("JAVA_HOME")
	if javaHome == "" {
		t.Skip("needs a JDK: JAVA_HOME is unset")
	}

	srv := fakeserver.Start(t, fakeserver.Options{})
	useFakeServer(t, srv)
	config.GlobalConfig.JavaHomePath = javaHome

	// meta-info.txt is written to the working directory.
	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(t.TempDir()))
	t.Cleanup(func() { os.Chdir(previous) })

	msg, ok, err := writeMetaInfo(11111, "test", srv.URLFor(fakeserver.Receiver), "tag1")
	require.NoError(t, err)
	require.True(t, ok, msg)

	requests := srv.Requests()
	require.Len(t, requests, 1)
	assert.Equal(t, "meta", requests[0].DT)

	body, err := requests[0].Body()
	require.NoError(t, err)
	assert.Contains(t, string(body), "processId=11111\nappName=test\n")
	assert.Regexp(t, `(?m)^javaVersion=\S`, string(body))
	assert.Regexp(t, `(?m)^tags=.*tag1`, string(body))
	assert.Empty(t, srv.Violations())
}

func TestComputeSystemTags(t *testing.T) {
	tests := []struct {
		name              string
		appRuntime        string
		nodejsCaptureMode string
		databaseTag       string
		expected          string
	}{
		{"dotnet", "dotnet", "", "", ".net"},
		{"dotnet ignores capture mode", "dotnet", "signal", "", ".net"},
		{"nodejs default hook mode (empty config)", "nodejs", "", "", "nodejs,hook-mode"},
		{"nodejs explicit hook mode", "nodejs", "hook", "", "nodejs,hook-mode"},
		{"nodejs signal mode", "nodejs", "signal", "", "nodejs,signal-mode"},
		{"nodejs signal mode is case/whitespace insensitive", "nodejs", "  SIGNAL  ", "", "nodejs,signal-mode"},
		{"nodejs unrecognized capture mode falls back to hook", "nodejs", "bogus", "", "nodejs,hook-mode"},
		{"java default runtime", "java", "", "", ""},
		{"unknown/empty appRuntime", "", "", "", ""},

		{"database-only run", "java", "", "postgres", "postgres"},
		{"database-only run, unknown runtime", "", "", "postgres", "postgres"},

		{"postgres alongside a nodejs capture merges", "nodejs", "signal", "postgres", "nodejs,signal-mode,postgres"},
		{"postgres alongside a dotnet capture merges", "dotnet", "", "postgres", ".net,postgres"},

		{"mysql database-only run", "", "", "mysql", "mysql"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, computeSystemTags(tt.appRuntime, tt.nodejsCaptureMode, tt.databaseTag))
		})
	}
}

func TestMergeTags(t *testing.T) {
	tests := []struct {
		name       string
		existing   string
		additional string
		expected   string
	}{
		{"both empty", "", "", ""},
		{"only existing", "tag1", "", "tag1"},
		{"only additional", "", "nodejs,hook-mode", "nodejs,hook-mode"},
		{"both present", "tag1", "nodejs,hook-mode", "tag1,nodejs,hook-mode"},
		{"multiple existing user tags preserved", "tag1,tag2", ".net", "tag1,tag2,.net"},
		{"trims stray leading/trailing commas on existing", ",tag1,", "nodejs", "tag1,nodejs"},
		{"trims stray leading/trailing commas on additional", "tag1", ",nodejs,", "tag1,nodejs"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, mergeTags(tt.existing, tt.additional))
		})
	}
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

func TestFullCaptureDoesNotStartOnceStopped(t *testing.T) {
	t.Cleanup(func() { stopping, stop = context.WithCancel(context.Background()) })

	dir := t.TempDir()
	previous, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(previous) })

	Stop()

	assert.Empty(t, FullCapture(0, "", false, "", ""))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no capture directory: the capture never began")

	waited := make(chan struct{})
	go func() {
		Wg.Wait()
		close(waited)
	}()

	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a capture that did not start is still counted, and Shutdown would wait on it forever")
	}
}
