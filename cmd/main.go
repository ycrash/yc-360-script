package main

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/user"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"shell"
)

// ------------------------------------------------------------------------------
//  Customer specific Properties
// ------------------------------------------------------------------------------

// Specify your JDK installation directory.
// var JAVA_HOME = "/usr/lib/jvm/java-11-openjdk-amd64"

// ------------------------------------------------------------------------------
//  Generic Properties
// ------------------------------------------------------------------------------
var (
	SCRIPT_VERSION      = "2019_07_04"
	SCRIPT_SPAN         = 120 // How long the whole script should take. Default=240
	JAVACORE_INTERVAL   = 30  // How often javacores should be taken. Default=30
	TOP_INTERVAL        = 60  // How often top data should be taken. Default=60
	TOP_DASH_H_INTERVAL = 5   // How often top dash H data should be taken. Default=5
	VMSTAT_INTERVAL     = 5   // How often vmstat data should be taken. Default=5
)

// ------------------------------------------------------------------------------
//  * All values are in seconds.
//  * All the 'INTERVAL' values should divide into the 'SCRIPT_SPAN' by a whole
//    integer to obtain expected results.
//  * Setting any 'INTERVAL' too low (especially JAVACORE) can result in data
//    that may not be useful towards resolving the issue.  This becomes a problem
//    when the process of collecting data obscures the real issue.
// ------------------------------------------------------------------------------

var (
	Pid           int
	YcServer      string
	ApiKey        string
	AppName       string
	GcLogFilePath string
	javaHome      string
)

func init() {
	flag.IntVar(&Pid, "p", 0, "Process Id, for example: 3121")
	flag.StringVar(&YcServer, "s", "", "YCrash Server URL, for example: https://ycrash.companyname.com")
	flag.StringVar(&ApiKey, "k", "", "API Key, for example: tier1app@12312-12233-1442134-112")
	flag.StringVar(&AppName, "a", "", "APP Name")
	flag.StringVar(&GcLogFilePath, "gc", "", "GC log file path")
	flag.StringVar(&javaHome, "j", "", "JAVA_HOME path, for example: /usr/lib/jvm/java-8-openjdk-amd64")
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
		output, err := GetGCLogFile()
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
	fscreen.WriteString(fmt.Sprintf("\n%s\n", NowString()))

	// Starting up
	mwriter := io.MultiWriter(fscreen, os.Stdout).(io.StringWriter)
	logger := Logger{writer: mwriter}
	logger.Log("yc script starting...")
	logger.Log("Script version: %s", SCRIPT_VERSION)

	// Display the PIDs which have been input to the script
	logger.Log("PROBLEMATIC_PID is: %d", Pid)

	// Display the being used in this script
	logger.Log("SCRIPT_SPAN = %d", SCRIPT_SPAN)
	logger.Log("JAVACORE_INTERVAL = %d", JAVACORE_INTERVAL)
	logger.Log("TOP_INTERVAL = %d", TOP_INTERVAL)
	logger.Log("TOP_DASH_H_INTERVAL = %d", TOP_DASH_H_INTERVAL)
	logger.Log("VMSTAT_INTERVAL = %d", VMSTAT_INTERVAL)

	// check if it can find gc log from ps
	var gc *os.File
	if len(GcLogFilePath) > 0 {
		gc, err = os.Open(GcLogFilePath)
		if err != nil {
			logger.Log("gc log file open failed %s", err.Error())
			GcLogFilePath = ""
		} else {
			defer gc.Close()
		}
	}
	var jstat shell.CmdHolder
	if len(GcLogFilePath) < 1 {
		gc, jstat, err = shell.CommandStartInBackgroundToFile("gc.log",
			shell.Command{path.Join(javaHome, "/bin/jstat"), "-gc", "-t", strconv.Itoa(Pid), "2000", "30"})
		if err != nil {
			return
		}
		defer gc.Close()
		GcLogFilePath = "gc.log"
		logger.Log("gc log set to %s", GcLogFilePath)
	}

	// Collect the user currently executing the script
	logger.Log("Collecting user authority data...")

	fwhoami, err := os.Create("whoami.out")
	if err != nil {
		return
	}
	defer fwhoami.Close()

	fwhoami.WriteString(fmt.Sprintf("%s\n", NowString()))
	current, err := user.Current()
	if err != nil {
		return
	}
	fwhoami.WriteString(fmt.Sprintf("%s\n", current.Username))

	logger.Log("Collection of user authority data complete.")

	// Create some of the output files with a blank line at top
	logger.Log("Creating output files...")
	vmstat, err := os.Create("vmstat.out")
	if err != nil {
		return
	}
	defer vmstat.Close()
	ps, err := os.Create("ps.out")
	if err != nil {
		return
	}
	defer ps.Close()
	top, err := os.Create("top.out")
	if err != nil {
		return
	}
	defer top.Close()
	topdash, err := os.Create(fmt.Sprintf("topdashH.%d.out", Pid))
	if err != nil {
		return
	}
	defer topdash.Close()
	logger.Log(`Output files created:
     vmstat.out
     ps.out
     top.out
     topdashH.%d.out`, Pid)

	// ------------------------------------------------------------------------------
	//                   Capture netstat x2
	// ------------------------------------------------------------------------------
	//  Collect the first netstat: date at the top, data, and then a blank line
	logger.Log("Collecting the first netstat snapshot...")
	netstat, err := os.Create("netstat.out")
	if err != nil {
		return
	}
	defer netstat.Close()
	netstat.WriteString(fmt.Sprintf("%s\n", NowString()))
	cmd := shell.NewCommand(shell.NetState)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return
	}
	_, err = netstat.Write(output)
	if err != nil {
		return
	}
	logger.Log("First netstat snapshot complete.")

	// ------------------------------------------------------------------------------
	//                   Capture top
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of top data...")
	top.WriteString(fmt.Sprintf("\n%s\n\n", NowString()))
	topCmd, err := shell.CommandStartInBackgroundWithWriter(top, shell.Top,
		"-d", strconv.Itoa(TOP_INTERVAL),
		"-n", strconv.Itoa(SCRIPT_SPAN/TOP_INTERVAL+1))
	if err != nil {
		return
	}
	logger.Log("Collection of top data started.")

	// ------------------------------------------------------------------------------
	//                   Capture top -H
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of top dash H data...")
	topdash.WriteString(fmt.Sprintf("\n%s\n\n", NowString()))
	topdash.WriteString(fmt.Sprintf("Collected against PID %d\n\n", Pid))
	topHCmd, err := shell.CommandStartInBackgroundWithWriter(topdash, shell.TopH,
		"-d", strconv.Itoa(TOP_DASH_H_INTERVAL),
		"-n", strconv.Itoa(SCRIPT_SPAN/TOP_DASH_H_INTERVAL+1),
		"-p", strconv.Itoa(Pid))
	if err != nil {
		return
	}

	logger.Log("Collection of top dash H data started for PID %d.", Pid)

	// ------------------------------------------------------------------------------
	//                   Capture vmstat
	// ------------------------------------------------------------------------------
	//  It runs in the background so that other tasks can be completed while this runs.
	logger.Log("Starting collection of vmstat data...")
	vmstat.WriteString(fmt.Sprintf("\n%s\n", NowString()))
	vmstatCmd, err := shell.CommandStartInBackgroundWithWriter(vmstat, shell.VMState,
		strconv.Itoa(VMSTAT_INTERVAL),
		strconv.Itoa(SCRIPT_SPAN/VMSTAT_INTERVAL+1))
	if err != nil {
		return
	}
	logger.Log("Collection of vmstat data started.")

	// ------------------------------------------------------------------------------
	//   				Capture thread dumps and ps
	// ------------------------------------------------------------------------------
	//  Initialize some loop variables
	m := SCRIPT_SPAN / JAVACORE_INTERVAL
	for n := 1; n <= m; n++ {
		// Collect a ps snapshot: date at the top, data, and then a blank line
		logger.Log("Collecting a ps snapshot...")
		ps.WriteString(fmt.Sprintf("\n%s\n", NowString()))
		cmd := shell.NewCommand(shell.PS)
		output, err = cmd.CombinedOutput()
		if err != nil {
			return
		}
		_, err = ps.Write(output)
		if err != nil {
			return
		}
		logger.Log("Collected a ps snapshot.")

		//  Collect a javacore against the problematic pid (passed in by the user)
		//  Javacores are output to the working directory of the JVM; in most cases this is the <profile_root>
		func() {
			logger.Log("Collecting thread dump...")
			jstack, err := shell.CommandCombinedOutputToFile(fmt.Sprintf("javacore.%d.out", n),
				shell.Command{path.Join(javaHome, "bin/jstack"), "-l", strconv.Itoa(Pid)})
			if err != nil {
				logger.Log("jstack error %s", err.Error())
				os.Exit(1)
				return
			}
			defer jstack.Close()
			logger.Log("Collected a thread dump for PID %d.", Pid)
		}()

		if n == m {
			break
		}
		// Pause for JAVACORE_INTERVAL seconds.
		logger.Log("sleeping for %d seconds...", JAVACORE_INTERVAL)
		time.Sleep(time.Second * time.Duration(JAVACORE_INTERVAL))
	}

	// ------------------------------------------------------------------------------
	//                Capture final netstat
	// ------------------------------------------------------------------------------
	logger.Log("Collecting the final netstat snapshot...")
	netstat.WriteString(fmt.Sprintf("\n%s\n", NowString()))
	cmd = shell.NewCommand(shell.NetState)
	output, err = cmd.CombinedOutput()
	if err != nil {
		return
	}
	_, err = netstat.Write(output)
	if err != nil {
		return
	}
	logger.Log("Final netstat snapshot complete.")

	// ------------------------------------------------------------------------------
	//  				Capture dmesg
	// ------------------------------------------------------------------------------
	logger.Log("Collecting other data.  This may take a few moments...")
	dmesg, err := shell.CommandCombinedOutputToFile("dmesg.out", shell.DMesg)
	if err != nil {
		return
	}
	defer dmesg.Close()
	// ------------------------------------------------------------------------------
	//  				Capture Disk Usage
	// ------------------------------------------------------------------------------
	df, err := os.Create("disk.out")
	if err != nil {
		return
	}
	defer df.Close()
	cmd = shell.NewCommand(shell.Disk)
	output, err = cmd.CombinedOutput()
	if err != nil {
		return
	}
	_, err = df.Write(output)
	if err != nil {
		return
	}

	logger.Log("Collected other data.")

	// -------------------------------
	// Compute transmitting parameters
	// -------------------------------
	parameters := fmt.Sprintf("de=%s&ts=%s", GetOutboundIP().String(), timestamp)
	// endpoint := fmt.Sprintf("%s/data-in?apiKey=%s&%s", YcServer, ApiKey, parameters)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", YcServer, ApiKey, parameters)
	var ok bool
	var msg string

	jstat.Wait()
	// stop started tasks
	err = topCmd.KillAndWait()
	if err != nil {
		return
	}
	err = topHCmd.KillAndWait()
	if err != nil {
		return
	}
	err = vmstatCmd.KillAndWait()
	if err != nil {
		return
	}

	// -------------------------------
	//     Transmit Top data
	// -------------------------------
	msg, ok = PostData(endpoint, "top", top)
	fmt.Printf(
		`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	// -------------------------------
	//     Transmit DF data
	// -------------------------------
	msg, ok = PostData(endpoint, "df", df)
	fmt.Printf(
		`DISK USAGE DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	// -------------------------------
	//     Transmit netstat data
	// -------------------------------
	msg, ok = PostData(endpoint, "ns", netstat)
	fmt.Printf(
		`NETSTAT DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	// -------------------------------
	//     Transmit ps data
	// -------------------------------
	msg, ok = PostData(endpoint, "ps", ps)
	fmt.Printf(
		`PROCESS STATUS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	// -------------------------------
	//     Transmit VMstat data
	// -------------------------------
	msg, ok = PostData(endpoint, "vmstat", vmstat)
	fmt.Printf(
		`VMstat DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok = PostData(endpoint, "gc", gc)
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
	// 1: concatenate individual thread dumps
	err = shell.CommandRun(shell.AppendJavaCoreFiles)
	if err != nil {
		return
	}
	// 2: Append top -H output file.
	err = shell.CommandRun(shell.AppendTopH, fmt.Sprintf("cat topdashH.%d.out >> ./threaddump.out", Pid))
	if err != nil {
		return
	}
	// 3: Transmit Thread dump
	td, err := os.Open("threaddump.out")
	if err != nil {
		return
	}
	defer td.Close()
	msg, ok = PostData(endpoint, "td", td)
	fmt.Printf(
		`THREAD DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)

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

func PostData(endpoint, dt string, file *os.File) (msg string, ok bool) {
	stat, err := file.Stat()
	if err != nil {
		panic(fmt.Errorf("file stat err %w", err))
	}
	fileName := stat.Name()
	if stat.Size() < 1 {
		msg = fmt.Sprintf("skipped empty file %s", fileName)
		return
	}

	url := fmt.Sprintf("%s&dt=%s", endpoint, dt)
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true,
	}
	httpClient := &http.Client{
		Transport: transport,
	}
	_, err = file.Seek(0, 0)
	if err != nil {
		panic(fmt.Errorf("file %s seek err %w", fileName, err))
	}
	resp, err := httpClient.Post(url, "Content-Type:text", file)
	if err != nil {
		msg = fmt.Sprintf("PostData post err %s", err.Error())
		return
	}

	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		msg = fmt.Sprintf("PostData get resp err %s", err.Error())
		return
	}
	msg = fmt.Sprintf("%s\nstatus code %d\n%s", url, resp.StatusCode, body)

	if resp.StatusCode == http.StatusOK {
		ok = true
	}
	return
}

func GetOutboundIP() net.IP {
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
	stamp := NowString()
	if len(values) == 0 {
		logger.writer.WriteString(stamp + format + "\n")
		return
	}
	logger.writer.WriteString(stamp + fmt.Sprintf(format, values...) + "\n")
}

func NowString() string {
	return time.Now().Format("Mon Jan 2 15:04:05 MST 2006 ")
}

func GetGCLogFile() (result string, err error) {
	output, err := shell.CommandCombinedOutput(shell.GC, fmt.Sprintf(`ps -f -p %d`, Pid))
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
		if len(submatch) > 2 {
			fp = submatch[1]
		}
	}
	if len(fp) < 1 {
		return
	}
	result = strings.TrimSpace(string(fp))
	return
}
