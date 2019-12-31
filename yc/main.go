package main

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"shell"
	"shell/capture"
	"shell/logger"
)

var (
	Pid      int
	YcServer string
	ApiKey   string
	AppName  string
	gcPath   string
	tdPath   string
	hdPath   string
	javaHome string
	heapDump bool
	version  bool
)

func init() {
	flag.IntVar(&Pid, "p", 0, "Process Id, for example: 3121")
	flag.StringVar(&YcServer, "s", "", "YCrash Server URL, for example: https://ycrash.companyname.com")
	flag.StringVar(&ApiKey, "k", "", "API Key, for example: tier1app@12312-12233-1442134-112")
	flag.StringVar(&AppName, "a", "", "APP Name")
	flag.StringVar(&gcPath, "gcPath", "", "GC log file path")
	flag.StringVar(&tdPath, "tdPath", "", "Thread dump log file path")
	flag.StringVar(&hdPath, "hdPath", "", "Heap dump log file path")
	flag.StringVar(&javaHome, "j", "", "JAVA_HOME path, for example: /usr/lib/jvm/java-8-openjdk-amd64")
	flag.BoolVar(&heapDump, "hd", false, "capture heap dumps")
	flag.BoolVar(&version, "version", false, "show version")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("No arguments are passed.")
		flag.Usage()
		return
	}

	flag.Parse()
	if version {
		fmt.Println("yc agent version: " + shell.SCRIPT_VERSION)
		os.Exit(0)
	}

	// must passed
	if Pid <= 0 {
		fmt.Println("Process id is not passed.")
		flag.Usage()
		return
	}

	if len(YcServer) < 1 {
		fmt.Println("YCrash Server URL is not passed")
		flag.Usage()
		return
	}
	if len(ApiKey) < 1 {
		fmt.Println("APIKey is not passed.")
		flag.Usage()
		return
	}
	if len(javaHome) < 1 {
		javaHome = os.Getenv("JAVA_HOME")
	}
	if len(javaHome) < 1 {
		fmt.Println("JAVA_HOME path is not passed")
		flag.Usage()
		return
	}

	// find gc log path in from command line arguments of ps result
	if len(gcPath) < 1 {
		output, err := getGCLogFile(Pid)
		if err == nil && len(output) > 0 {
			gcPath = output
		}
	}

	fmt.Printf("PID is %d\n", Pid)
	fmt.Printf("YC_SERVER is %s\n", YcServer)
	fmt.Printf("API_KEY is %s\n", ApiKey)
	fmt.Printf("APP_NAME is %s\n", AppName)
	fmt.Printf("JAVA_HOME is %s\n", javaHome)
	fmt.Printf("GC_LOG is %s\n\n", gcPath)

	var err error
	defer func() {
		if err != nil {
			fmt.Printf("Unexpected Error %s\n", err)
			panic(err)
		}
	}()
	// -------------------------------------------------------------------
	//  Create output files
	// -------------------------------------------------------------------
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
	// endpoint := fmt.Sprintf("%s/data-in?apiKey=%s&%s", YcServer, ApiKey, parameters)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", YcServer, ApiKey, parameters)

	dname := "yc-" + timestamp
	err = os.Mkdir(dname, 0777)
	if err != nil {
		return
	}

	err = os.Chdir(dname)
	if err != nil {
		return
	}

	// Create the screen.out and put the current date in it.
	fscreen, err := os.Create("screen.out")
	if err != nil {
		return
	}
	defer fscreen.Close()

	// Starting up
	mwriter := io.MultiWriter(fscreen, os.Stdout).(io.StringWriter)
	logger.SetStringWriter(mwriter)
	logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
	logger.Log("yc script starting...")
	logger.Log("Script version: %s", shell.SCRIPT_VERSION)

	// Display the PIDs which have been input to the script
	logger.Log("PROBLEMATIC_PID is: %d", Pid)

	// Display the being used in this script
	logger.Log("SCRIPT_SPAN = %d", shell.SCRIPT_SPAN)
	logger.Log("JAVACORE_INTERVAL = %d", shell.JAVACORE_INTERVAL)
	logger.Log("TOP_INTERVAL = %d", shell.TOP_INTERVAL)
	logger.Log("TOP_DASH_H_INTERVAL = %d", shell.TOP_DASH_H_INTERVAL)
	logger.Log("VMSTAT_INTERVAL = %d", shell.VMSTAT_INTERVAL)

	// check if it can find gc log from ps
	var gc *os.File
	gc, err = processGCLogFile(gcPath, "gc.log")
	if err != nil {
		logger.Log("process log file failed %s, err: %s", gcPath, err.Error())
	}
	var jstat shell.CmdHolder
	if gc == nil {
		gc, jstat, err = shell.CommandStartInBackgroundToFile("gc.log",
			shell.Command{path.Join(javaHome, "/bin/jstat"), "-gc", "-t", strconv.Itoa(Pid), "2000", "30"})
		if err != nil {
			return
		}
		gcPath = "gc.log"
		logger.Log("gc log set to %s", gcPath)
	}
	defer gc.Close()

	// Collect the user currently executing the script
	logger.Log("Collecting user authority data...")

	fwhoami, err := os.Create("whoami.out")
	if err != nil {
		return
	}
	defer fwhoami.Close()

	fwhoami.WriteString(fmt.Sprintf("%s\n", nowString()))
	current, err := user.Current()
	if err != nil {
		return
	}
	fwhoami.WriteString(fmt.Sprintf("%s\n", current.Username))

	logger.Log("Collection of user authority data complete.")

	// ------------------------------------------------------------------------------
	//                   Capture netstat x2
	// ------------------------------------------------------------------------------
	//  Collect the first netstat: date at the top, data, and then a blank line
	logger.Log("Collecting the first netstat snapshot...")
	capNetStat := capture.NewNetStat()
	netStat := goCapture(endpoint, capture.WrapRun(capNetStat))
	logger.Log("First netstat snapshot complete.")

	// ------------------------------------------------------------------------------
	//                   Capture top
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of top data...")
	capTop := &capture.Top{}
	top := goCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")

	// ------------------------------------------------------------------------------
	//                   Capture vmstat
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of vmstat data...")
	capVMStat := &capture.VMStat{}
	vmstat := goCapture(endpoint, capture.WrapRun(capVMStat))
	logger.Log("Collection of vmstat data started.")

	// ------------------------------------------------------------------------------
	//   				Capture thread dumps and ps
	// ------------------------------------------------------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid:      Pid,
		TdPath:   tdPath,
		JavaHome: javaHome,
	}
	threadDump := goCapture(endpoint, capture.WrapRun(capThreadDump))
	//  Initialize some loop variables
	m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
	capPS := capture.NewPS()
	ps := goCapture(endpoint, capture.WrapRun(capPS))
	logger.Log("Collecting ps snapshot...")
	for n := 1; n <= m; n++ {
		// Collect a ps snapshot: date at the top, data, and then a blank line
		capPS.Continue()

		if n == m {
			break
		}
		// Pause for JAVACORE_INTERVAL seconds.
		logger.Log("sleeping for %d seconds...", shell.JAVACORE_INTERVAL)
		time.Sleep(time.Second * time.Duration(shell.JAVACORE_INTERVAL))
	}
	logger.Log("Collected ps snapshot.")

	// ------------------------------------------------------------------------------
	//                Capture final netstat
	// ------------------------------------------------------------------------------
	logger.Log("Collecting the final netstat snapshot...")
	capNetStat.Done()
	logger.Log("Final netstat snapshot complete.")

	// ------------------------------------------------------------------------------
	//  				Capture dmesg
	// ------------------------------------------------------------------------------
	logger.Log("Collecting other data.  This may take a few moments...")
	// There is no endpoint for this now.
	// dmesg := goCapture(endpoint, captureDMesg)
	// ------------------------------------------------------------------------------
	//  				Capture Disk Usage
	// ------------------------------------------------------------------------------
	disk := goCapture(endpoint, capture.WrapRun(&capture.Disk{}))

	logger.Log("Collected other data.")

	var ok bool
	var msg string

	jstat.Wait()
	// stop started tasks
	err = capTop.Kill()
	if err != nil {
		return
	}
	err = capVMStat.Kill()
	if err != nil {
		return
	}
	capPS.Kill()

	// -------------------------------
	//     Transmit Top data
	// -------------------------------
	result := <-top
	fmt.Printf(
		`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit DF data
	// -------------------------------
	result = <-disk
	fmt.Printf(
		`DISK USAGE DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit netstat data
	// -------------------------------
	result = <-netStat
	fmt.Printf(
		`NETSTAT DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit ps data
	// -------------------------------
	result = <-ps
	fmt.Printf(
		`PROCESS STATUS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit VMstat data
	// -------------------------------
	result = <-vmstat
	fmt.Printf(
		`VMstat DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok = postData(endpoint, "gc", gc)
	fmt.Printf(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, gcPath, ok, msg)

	// -------------------------------
	//     Transmit Thread dump
	// -------------------------------
	result = <-threadDump
	fmt.Printf(
		`THREAD DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)

	// -------------------------------
	//     Transmit MetaInfo
	// -------------------------------
	msg, ok, err = writeMetaInfo(Pid, AppName, endpoint)
	if err != nil {
		logger.Log("writeMetaInfo failed %s", err.Error())
	}
	fmt.Printf(
		`META INFO DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	// -------------------------------
	//     Transmit Heap dump result
	// -------------------------------
	ep := fmt.Sprintf("%s/yc-receiver-heap?apiKey=%s&%s", YcServer, ApiKey, parameters)
	capHeapDump := capture.NewHeapDump(javaHome, Pid, hdPath, heapDump)
	capHeapDump.SetEndpoint(ep)
	hdResult, err := capHeapDump.Run()
	if err != nil {
		logger.Log("heap dump err %s", err.Error())
	}
	fmt.Printf(
		`HEAP DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, hdResult.Ok, hdResult.Msg)
	// -------------------------------
	//     Conclusion
	// -------------------------------
	requestFin(YcServer, ApiKey, parameters)

	ou := strings.SplitN(ApiKey, "@", 2)[0]
	reportEndpoint := fmt.Sprintf("%s/yc-report.jsp?ou=%s&%s", YcServer, ou, parameters)
	fmt.Printf(`
See the report: %s
--------------------------------
`, reportEndpoint)
}

func requestFin(server, apiKey, parameters string) (err error) {
	finEp := fmt.Sprintf("%s/yc-fin?apiKey=%s&%s", server, apiKey, parameters)
	post, err := http.Post(finEp, "text", nil)
	if err == nil {
		defer post.Body.Close()
		var r []byte
		r, err = ioutil.ReadAll(post.Body)
		if err == nil {
			fmt.Printf(
				`yc-fin endpoint: %s
Resp: %s

--------------------------------
`, finEp, r)
		}
	}
	if err != nil {
		logger.Log("post yc-fin err %s", err.Error())
	}
	return
}

var postData = shell.PostData
var nowString = shell.NowString
var getOutboundIP = shell.GetOutboundIP
var goCapture = capture.GoCapture

func getGCLogFile(pid int) (result string, err error) {
	output, err := shell.CommandCombinedOutput(shell.GC, fmt.Sprintf(`ps -f -p %d`, pid))
	if err != nil {
		return
	}
	re := regexp.MustCompile("-Xlog:gc.+? ")
	loggc := re.Find(output)

	var fp []byte
	if len(loggc) > 1 {
		fre := regexp.MustCompile("file=(.+?):")
		submatch := fre.FindSubmatch(loggc)
		if len(submatch) >= 2 {
			fp = submatch[1]
		} else {
			fre := regexp.MustCompile("gc:((.+?)$|(.+?):)")
			submatch := fre.FindSubmatch(loggc)
			if len(submatch) >= 2 {
				fp = submatch[1]
			}
		}
	} else {
		re := regexp.MustCompile("-Xloggc:(.+?) ")
		submatch := re.FindSubmatch(output)
		if len(submatch) >= 2 {
			fp = submatch[1]
		}
	}
	if len(fp) < 1 {
		return
	}
	result = strings.TrimSpace(string(fp))
	return
}

func processGCLogFile(gcPath string, out string) (gc *os.File, err error) {
	if len(gcPath) <= 0 {
		return
	}
	gcf, err := os.Open(gcPath)
	// gcPath exists, cp it
	if err == nil {
		defer gcf.Close()
		gc, err = os.Create(out)
		if err != nil {
			return
		}
		_, err = io.Copy(gc, gcf)
		return
	}
	logger.Log("collecting rotation gc logs, because file open failed %s", err.Error())
	// err is other than not exists
	if !os.IsNotExist(err) {
		return
	}

	// gcPath is not exists, maybe using -XX:+UseGCLogFileRotation
	d := filepath.Dir(gcPath)
	logName := filepath.Base(gcPath)
	open, err := os.Open(d)
	if err != nil {
		return nil, err
	}
	defer open.Close()
	fs, err := open.Readdirnames(0)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(logName + "\\.([0-9]+?)\\.current")
	reo := regexp.MustCompile(logName + "\\.([0-9]+)")
	var rf []string
	files := make([]int, 0, len(fs))
	for _, f := range fs {
		r := re.FindStringSubmatch(f)
		if len(r) > 1 {
			rf = r
			continue
		}
		r = reo.FindStringSubmatch(f)
		if len(r) > 1 {
			p, err := strconv.Atoi(r[1])
			if err != nil {
				logger.Log("skipped file %s because can not parse its index", f)
				continue
			}
			files = append(files, p)
		}
	}
	if len(rf) < 2 {
		err = fmt.Errorf("can not find the current log file, %w", os.ErrNotExist)
		return
	}
	p, err := strconv.Atoi(rf[1])
	if err != nil {
		return
	}
	gc, err = os.Create(out)
	if err != nil {
		return
	}
	// try to find previous log
	var preLog string
	if len(files) == 1 {
		preLog = gcPath + "." + strconv.Itoa(files[0])
	} else if len(files) > 1 {
		files = append(files, p)
		sort.Ints(files)
		index := -1
		for i, file := range files {
			if file == p {
				index = i
				break
			}
		}
		if index >= 0 {
			if index-1 >= 0 {
				preLog = gcPath + "." + strconv.Itoa(files[index-1])
			} else {
				preLog = gcPath + "." + strconv.Itoa(files[len(files)-1])
			}
		}
	}
	if len(preLog) > 0 {
		logger.Log("collecting previous gc log %s", preLog)
		err = copyFile(gc, preLog)
		if err != nil {
			logger.Log("failed to collect previous gc log %s", err.Error())
		} else {
			logger.Log("collected previous gc log %s", preLog)
		}
	}

	curLog := filepath.Join(d, rf[0])
	logger.Log("collecting previous gc log %s", curLog)
	err = copyFile(gc, curLog)
	if err != nil {
		logger.Log("failed to collect previous gc log %s", err.Error())
	} else {
		logger.Log("collected previous gc log %s", curLog)
	}
	return
}

// combine previous gc log to new gc log
func copyFile(gc *os.File, file string) (err error) {
	log, err := os.Open(file)
	if err != nil {
		return
	}
	defer log.Close()
	_, err = io.Copy(gc, log)
	return
}

const metaInfoTemplate = `hostName=%s
processId=%d
appName=%s`

func writeMetaInfo(processId int, appName, endpoint string) (msg string, ok bool, err error) {
	file, err := os.Create("meta-info.txt")
	if err != nil {
		return
	}
	defer file.Close()
	hostname, err := os.Hostname()
	if err != nil {
		return
	}
	_, err = io.Copy(file, bytes.NewBufferString(fmt.Sprintf(metaInfoTemplate, hostname, processId, appName)))
	if err != nil {
		return
	}
	msg, ok = postData(endpoint, "meta", file)
	return
}

type CaptureResult = capture.Result

func captureDMesg(endpoint string, c chan CaptureResult) {
	var err error
	result := CaptureResult{}
	defer func() {
		if err != nil {
			result.Msg = fmt.Sprintf("capture failed: %s", err.Error())
		}
		c <- result
		close(c)
	}()
	dmesg, err := shell.CommandCombinedOutputToFile("dmesg.out", shell.DMesg)
	if err != nil {
		return
	}
	defer dmesg.Close()
	result.Msg, result.Ok = postData(endpoint, "dmesg", dmesg)
	return
}
