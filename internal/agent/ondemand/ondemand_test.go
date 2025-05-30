package ondemand

import (
	"fmt"
	"os"
	"testing"
	"time"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"
)

const (
	api  = "tier1app@12312-12233-1442134-112"
	host = "https://test.gceasy.io"
)

func init() {
	// err := os.Chdir("testdata")
	// if err != nil {
	// 	panic(err)
	// }
}

func TestFindGCLog(t *testing.T) {
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
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", capture.GetOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", host, api, parameters)

	t.Run("requestFin", func(t *testing.T) {
		finEp := fmt.Sprintf("%s/yc-fin?apiKey=%s&%s", host, api, parameters)
		RequestFin(finEp)
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

	msg, ok := capture.PostData(endpoint, "top", top)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "df", df)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "ns", netstat)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "ps", ps)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "vmstat", vmstat)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "gc", gc)
	if !ok {
		t.Fatal("post data failed", msg)
	}
	msg, ok = capture.PostData(endpoint, "td", td)
	if !ok {
		t.Fatal("post data failed", msg)
	}
}

func TestWriteMetaInfo(t *testing.T) {
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

	// Arrange

	var op = "ProcessId java  -Xlog:gc*=info,gc+heap=debug,gc+ref*=debug,gc+ergo*=trace,gc+age*=trace:file=/tmp/gc.log:utctime,pid,level,tags:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	var logFile = ""

	expected := "/tmp/gc.log"

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 1: -Xlog:gc:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 2: -Xlog:gc:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 3: -Xlog:gc+heap=info:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=info:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 4: -Xlog:gc+heap=info:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+ref=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 5: -Xlog:gc+heap=info:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+ref=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 6: -Xlog:gc+age=trace:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+age=trace:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 7: -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=debug:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=debug:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 8: -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 9: -Xlog:gc*:file=/tmp/gc.log:time,tags

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:time,tags -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 10: -Xlog:gc*:file=/tmp/gc.log:utctime

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 11: -Xlog:gc*:file=/tmp/gc.log:uptime,level

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:uptime,level -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 12: -Xlog:gc*:file=/tmp/gc.log:utctime,pid

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime,pid -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 13: -Xlog:gc*:file=/tmp/gc.log:utctime,pid,level,tags

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:utctime,pid,level,tags -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 14: -Xlog:gc*:file=/tmp/gc.log:uptime,level:filecount=2,filesize=100M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*:file=/tmp/gc.log:uptime,level:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 15: -Xlog:gc*=debug:file=/tmp/gc.log:time,pid,level:filecount=10,filesize=20M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=debug:file=/tmp/gc.log:time,pid,level:filecount=10,filesize=20M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 16: -Xlog:gc*=trace:file=/tmp/gc.log:uptime,tags:filecount=3,filesize=5M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=trace:file=/tmp/gc.log:uptime,tags:filecount=3,filesize=5M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 17: -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log:utctime,level,filecount=4,filesize=50M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+age=trace:file=/tmp/gc.log:utctime,level,filecount=4,filesize=50M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 18: -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 19: -Xlog:gc+heap=debug,gc+phases=debug,gc+humongous=debug:file=/tmp/gc.log:uptime,tags:filecount=5,filesize=25M

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info,gc+ref*=debug,gc+ergo*=trace,gc+age*=trace:file=/tmp/gc.log:utctime,pid,level,tags:filecount=2,filesize=100M -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

	// case 20: -Xlog:gc*=info:file=/tmp/gc.log:noforcetty

	op = "ProcessId java  -Xlog:gc*=debug:stdout -Xlog:gc*=info:file=/tmp/gc.log:noforcetty -Xms2g -Xmx4g -Xss40m -Duser.language=en -Duser.country=en_US -DhprofStrictnessWarning=true -DlogDir=\"D:\tier1appdevelopment\" -DuploadDir=\"D:\tier1appdevelopment\" -DonlyTroubleshootingReport=true -Dapp=yc -jar webapp-runner.jar -AconnectionTimeout=3600000 --secure-error-report-valve --port 8080 yc.war  11232"

	logFile = ""

	// Act

	logFile = ExtractGCLogPathFromCmdline(op)

	// Assert

	if logFile != expected {

		t.Fatalf("error %s %s", expected, logFile)

	}

}
