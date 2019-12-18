package main

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import (
	"archive/zip"
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shell"
	"shell/capture"
)

var (
	Pid           int
	YcServer      string
	ApiKey        string
	AppName       string
	GcLogFilePath string
	javaHome      string
	heapDump      bool

	logger Logger
)

func init() {
	flag.IntVar(&Pid, "p", 0, "Process Id, for example: 3121")
	flag.StringVar(&YcServer, "s", "", "YCrash Server URL, for example: https://ycrash.companyname.com")
	flag.StringVar(&ApiKey, "k", "", "API Key, for example: tier1app@12312-12233-1442134-112")
	flag.StringVar(&AppName, "a", "", "APP Name")
	flag.StringVar(&GcLogFilePath, "gc", "", "GC log file path")
	flag.StringVar(&javaHome, "j", "", "JAVA_HOME path, for example: /usr/lib/jvm/java-8-openjdk-amd64")
	flag.BoolVar(&heapDump, "hd", false, "capture heap dumps")
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("No arguments are passed.")
		flag.Usage()
		return
	}

	flag.Parse()

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

	// can be ignored
	if len(GcLogFilePath) < 1 {
		output, err := getGCLogFile(Pid)
		if err == nil && len(output) > 0 {
			GcLogFilePath = output
		}
	}

	fmt.Printf("PID is %d\n", Pid)
	fmt.Printf("YC_SERVER is %s\n", YcServer)
	fmt.Printf("API_KEY is %s\n", ApiKey)
	fmt.Printf("APP_NAME is %s\n", AppName)
	fmt.Printf("JAVA_HOME is %s\n", javaHome)
	fmt.Printf("GC_LOG is %s\n\n", GcLogFilePath)

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
	fscreen.WriteString(fmt.Sprintf("\n%s\n", nowString()))

	// Starting up
	mwriter := io.MultiWriter(fscreen, os.Stdout).(io.StringWriter)
	logger = Logger{writer: mwriter}
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
	gc, err = processGCLogFile(GcLogFilePath, "gc.log")
	if err != nil {
		logger.Log("process log file failed %s", GcLogFilePath)
	}
	var jstat shell.CmdHolder
	if gc == nil {
		gc, jstat, err = shell.CommandStartInBackgroundToFile("gc.log",
			shell.Command{path.Join(javaHome, "/bin/jstat"), "-gc", "-t", strconv.Itoa(Pid), "2000", "30"})
		if err != nil {
			return
		}
		GcLogFilePath = "gc.log"
		logger.Log("gc log set to %s", GcLogFilePath)
	}
	defer gc.Close()

	var hdResultChan chan CaptureResult
	if heapDump {
		ep := fmt.Sprintf("%s/yc-receiver-heap?apiKey=%s&%s", YcServer, ApiKey, parameters)
		hdResultChan = captureHeapDump(ep, Pid, javaHome)
	}

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
	capNetStat := &capture.NetStat{}
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
	//                   Capture top -H
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of top dash H data...")
	capTopH := &capture.TopH{
		Pid: Pid,
	}
	goCapture(endpoint, capture.WrapRun(capTopH))
	logger.Log("Collection of top dash H data started for PID %d.", Pid)

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
	//  Initialize some loop variables
	m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
	capPS := capture.NewPS()
	ps := goCapture(endpoint, capture.WrapRun(capPS))
	capJStack := capture.NewJStack(javaHome, Pid)
	go func() {
		_, err := capJStack.Run()
		if err != nil {
			logger.Log("jstack error %s", err.Error())
		}
	}()
	logger.Log("Collecting ps snapshot and thread dump...")
	for n := 1; n <= m; n++ {
		// Collect a ps snapshot: date at the top, data, and then a blank line
		capPS.Continue()

		//  Collect a javacore against the problematic pid (passed in by the user)
		//  Javacores are output to the working directory of the JVM; in most cases this is the <profile_root>
		capJStack.Continue()

		if n == m {
			break
		}
		// Pause for JAVACORE_INTERVAL seconds.
		logger.Log("sleeping for %d seconds...", shell.JAVACORE_INTERVAL)
		time.Sleep(time.Second * time.Duration(shell.JAVACORE_INTERVAL))
	}
	logger.Log("Collected ps snapshot and thread dump.")

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
	err = capTopH.Kill()
	if err != nil {
		return
	}
	err = capVMStat.Kill()
	if err != nil {
		return
	}
	capPS.Kill()
	capJStack.Kill()

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
`, GcLogFilePath, ok, msg)

	// -------------------------------
	//     Transmit Thread dump
	// -------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid: Pid,
	}
	result = <-goCapture(endpoint, capture.WrapRun(capThreadDump))
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
	if hdResultChan != nil {
		hdResult := <-hdResultChan
		fmt.Printf(
			`HEAP DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, hdResult.Ok, hdResult.Msg)
	}
	// -------------------------------
	//     Conclusion
	// -------------------------------
	ou := strings.SplitN(ApiKey, "@", 2)[0]
	reportEndpoint := fmt.Sprintf("%s/yc-report.jsp?ou=%s&%s", YcServer, ou, parameters)
	fmt.Printf(`
See the report: %s
--------------------------------
`, reportEndpoint)
}

var postData = shell.PostData
var nowString = shell.NowString

func getOutboundIP() net.IP {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		panic(err)
	}
	defer conn.Close()

	localAddr := conn.LocalAddr().(*net.UDPAddr)

	return localAddr.IP
}

type Logger struct {
	writer io.StringWriter
}

func (logger *Logger) Log(format string, values ...interface{}) {
	stamp := nowString()
	if len(values) == 0 {
		logger.writer.WriteString(stamp + format + "\n")
		return
	}
	logger.writer.WriteString(stamp + fmt.Sprintf(format, values...) + "\n")
}

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

func processGCLogFile(log string, out string) (gc *os.File, err error) {
	if len(log) <= 0 {
		return
	}
	gc, err = os.Open(log)
	if err == nil {
		return
	}
	logger.Log("gc log file open failed %s", err.Error())
	if !os.IsNotExist(err) {
		return
	}
	d := filepath.Dir(log)
	open, err := os.Open(d)
	if err != nil {
		return nil, err
	}
	defer open.Close()
	fs, err := open.Readdirnames(0)
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(log + "\\.([0-9])\\.current")
	for _, f := range fs {
		rf := re.FindStringSubmatch(f)
		if len(rf) > 1 {
			err = func() error {
				ogc, err := os.Open(rf[0])
				if err != nil {
					return err
				}
				defer ogc.Close()
				p, err := strconv.Atoi(rf[1])
				if err != nil {
					return err
				}
				if p-1 >= 0 {
					p = p - 1
				} else {
					err = fmt.Errorf("invalid gc log index %d", p)
					return err
				}
				opgc, err := os.Open(log + "." + strconv.Itoa(p))
				if err != nil {
					return err
				}
				defer opgc.Close()
				gc, err = os.Create(out)
				if err != nil {
					return err
				}
				// combine previous gc log to new gc log
				_, err = io.Copy(gc, opgc)
				if err != nil {
					return err
				}
				_, err = io.Copy(gc, ogc)
				if err != nil {
					return err
				}
				return nil
			}()
			if err != nil {
				return nil, err
			}
			return gc, nil
		}
	}
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

func captureHeapDump(endpoint string, pid int, javaHome string) (c chan CaptureResult) {
	c = make(chan CaptureResult)
	go func() {
		var err error
		result := CaptureResult{}
		defer func() {
			if err != nil {
				result.Msg = fmt.Sprintf("capture heap dump failed: %s", err.Error())
			}
			c <- result
			close(c)
		}()
		logger.Log("capturing heap dump...")
		dir, err := os.Getwd()
		if err != nil {
			return
		}
		output, err := shell.CommandCombinedOutput(shell.Command{path.Join(javaHome, "/bin/jcmd"), strconv.Itoa(pid), "GC.heap_dump", filepath.Join(dir, "/heap_dump.out")})
		if err != nil {
			if len(output) > 1 {
				err = fmt.Errorf("%w because %s", err, output)
			}
			return
		}
		logger.Log("captured heap dump.")
		zipfile, err := os.Create("heap_dump.zip")
		if err != nil {
			logger.Log("failed to create zip file")
			return
		}
		defer zipfile.Close()
		writer := zip.NewWriter(bufio.NewWriter(zipfile))
		out, err := writer.Create("heap_dump.out")
		if err != nil {
			logger.Log("failed to create zip file")
			return
		}
		hdout, err := os.Open("heap_dump.out")
		if err != nil {
			logger.Log("failed to open heap dump file")
			return
		}
		defer hdout.Close()
		_, err = io.Copy(out, hdout)
		if err != nil {
			logger.Log("failed to zip heap dump file")
			return
		}
		err = writer.Close()
		if err != nil {
			logger.Log("failed to finish zipping heap dump file")
			return
		}

		logger.Log("zipped heap dump.")
		result.Msg, result.Ok = postData(endpoint, "hd&Content-Encoding=zip", zipfile)
	}()
	return
}

func goCapture(endpoint string, fn func(endpoint string, c chan CaptureResult)) (c chan CaptureResult) {
	c = make(chan CaptureResult)
	go fn(endpoint, c)
	return
}

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
