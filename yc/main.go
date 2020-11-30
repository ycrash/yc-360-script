package main

// Change History
// Dec' 02, 2019: Zhi : Initial Draft
// Dec' 05, 2019: Ram : Passing JAVA_HOME as parameter to the program instead of hard-coding in the program.
//                      Changed yc end point
//                      Changed minor changes to messages printed on the screen

import (
	"bytes"
	"errors"
	sfmt "fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"shell"
	"shell/capture"
	"shell/config"
	"shell/fmt"
	"shell/logger"

	"github.com/gentlemanautomaton/cmdline"
)

var wg sync.WaitGroup

func main() {
	err := config.ParseFlags(os.Args)
	if err != nil {
		fmt.Println(err.Error())
		os.Exit(1)
	}

	osSig := make(chan os.Signal, 1)
	signal.Notify(osSig, syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)

	go mainLoop()

	select {
	case <-osSig:
		fmt.Println("Waiting...")
		wg.Wait()
	}
}

func mainLoop() {
	if len(os.Args) < 2 {
		fmt.Println("No arguments are passed.")
		config.ShowUsage()
		os.Exit(1)
	}

	if config.GlobalConfig.ShowVersion {
		fmt.Println("yc agent version: " + shell.SCRIPT_VERSION)
		os.Exit(0)
	}

	if len(config.GlobalConfig.Server) < 1 {
		fmt.Println("'-s' yCrash server URL argument not passed.")
		config.ShowUsage()
		os.Exit(1)
	}
	if len(config.GlobalConfig.ApiKey) < 1 {
		fmt.Println("'-k' yCrash API Key argument not passed.")
		config.ShowUsage()
		os.Exit(1)
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		config.GlobalConfig.JavaHomePath = os.Getenv("JAVA_HOME")
	}
	if len(config.GlobalConfig.JavaHomePath) < 1 {
		fmt.Println("'-j' yCrash JAVA_HOME argument not passed.")
		config.ShowUsage()
		os.Exit(1)
	}
	if config.GlobalConfig.M3 && config.GlobalConfig.OnlyCapture {
		logger.Log("WARNING: -onlyCapture will be ignored in m3 mode.")
		config.GlobalConfig.OnlyCapture = false
	}
	msg, ok := shell.StartupAttend()
	fmt.Printf(
		`startup attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)

	if config.GlobalConfig.Port > 0 {
		go func() {
			s, err := shell.NewServer(config.GlobalConfig.Address, config.GlobalConfig.Port)
			if err != nil {
				logger.Log("WARNING: %s", err)
				return
			}
			s.ProcessPids = processPids
			err = s.Serve()
			if err != nil {
				logger.Log("WARNING: %s", err)
			}
		}()
	}

	if config.GlobalConfig.M3 {
		go func() {
			for {
				time.Sleep(config.GlobalConfig.M3Frequency)

				timestamp := time.Now().Format("2006-01-02T15-04-05")
				parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
				endpoint := fmt.Sprintf("%s/m3-receiver?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)
				err := process(timestamp, endpoint)
				if err != nil {
					logger.Log("WARNING: process failed, %s", err)
					continue
				}

				finEp := fmt.Sprintf("%s/m3-fin?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)
				resp, err := requestFin(finEp)
				if err != nil {
					logger.Log("WARNING: Request M3 Fin failed, %s", err)
					continue
				}
				if len(resp) <= 0 {
					logger.Log("WARNING: skip empty resp")
					continue
				}
				err = processResp(resp)
				if err != nil {
					logger.Log("WARNING: processResp failed, %s", err)
					continue
				}
			}
		}()
	} else if config.GlobalConfig.Pid > 0 {
		fullProcess(config.GlobalConfig.Pid)
		os.Exit(0)
	} else if config.GlobalConfig.Port <= 0 && !config.GlobalConfig.M3 {
		logger.Log("WARNING: nothing can be done")
		os.Exit(1)
	}
	for {
		msg, ok := shell.Attend()
		fmt.Printf(
			`daily attendance task
Is completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}
}

func processResp(resp []byte) (err error) {
	pids, err := shell.ParseJsonResp(resp)
	if err != nil {
		logger.Log("WARNING: Get PID from ParseJsonResp failed, %s", err)
		return
	}
	return processPids(pids)
}

// only one thread can run capture process
var one sync.Mutex

func processPids(pids []int) (err error) {
	one.Lock()
	defer one.Unlock()

	if len(pids) <= 0 {
		logger.Log("No action needed.")
		return
	}
	set := make(map[int]struct{}, len(pids))
	for _, pid := range pids {
		if _, ok := set[pid]; ok {
			continue
		}
		set[pid] = struct{}{}
		if len(config.GlobalConfig.CaptureCmd) > 0 {
			_, err := runCaptureCmd(pid, config.GlobalConfig.CaptureCmd)
			if err != nil {
				logger.Log("WARNING: runCaptureCmd failed %s", err)
				continue
			}
		} else {
			fullProcess(pid)
		}
	}
	return
}

func process(timestamp string, endpoint string) (err error) {
	one.Lock()
	defer one.Unlock()

	dname := "yc-" + timestamp
	err = os.Mkdir(dname, 0777)
	if err != nil {
		return
	}
	wg.Add(1)
	defer func() {
		defer wg.Done()
		err := os.RemoveAll(dname)
		if err != nil {
			logger.Log("WARNING: Can not remove the current directory: %s", err)
			return
		}
	}()
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	defer os.Chdir(dir)
	err = os.Chdir(dname)
	if err != nil {
		return
	}

	logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	pids, err := shell.GetProcessIds(config.GlobalConfig.ProcessTokens)
	if err == nil && len(pids) > 0 {
		set := make(map[int]struct{}, len(pids))
		for i := 0; i < len(pids); i++ {
			pid := pids[i]
			if _, ok := set[pid]; ok {
				continue
			}
			set[pid] = struct{}{}
			logger.Log("uploading gc log for pid %d", pid)
			uploadGCLog(endpoint, pid)
		}
	} else {
		logger.Log("WARNING: No PID has ProcessTokens or failed by error %v", err)
	}

	logger.Log("Starting collection of top data...")
	capTop := &capture.Top4M3{}
	top := goCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")
	if top != nil {
		result := <-top
		fmt.Printf(
			`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}
	return
}

func uploadGCLog(endpoint string, pid int) {
	var gcp string
	bs, err := runGCCaptureCmd(pid)
	if err == nil && len(bs) > 0 {
		gcp = string(bs)
	} else {
		output, err := getGCLogFile(pid)
		if err == nil && len(output) > 0 {
			gcp = output
		}
	}
	var gc *os.File
	fn := fmt.Sprintf("gc.%d.log", pid)
	gc, err = processGCLogFile(gcp, fn)
	if err != nil {
		logger.Log("process log file failed %s, err: %s", gcp, err.Error())
	}
	var jstat shell.CmdManager
	if gc == nil || err != nil {
		gc, jstat, err = shell.CommandStartInBackgroundToFile(fn,
			shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"})
		if err == nil {
			gcp = fn
			logger.Log("gc log set to %s", gcp)
		} else {
			logger.Log("WARNING: failed to capture gc log: %s", err.Error())
		}
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()
	if jstat != nil {
		jstat.Wait()
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok := shell.PostCustomDataWithPositionFunc(endpoint, fmt.Sprintf("dt=gc&pid=%d", pid), gc, shell.PositionLast5000Lines)
	absGCPath, err := filepath.Abs(gcp)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", gcp, err.Error())
	}
	fmt.Printf(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absGCPath, ok, msg)
}

func fullProcess(pid int) {
	updatePaths(pid)
	pidPassed := true
	if pid <= 0 {
		pidPassed = false
	}

	// find gc log path in from command line arguments of ps result
	if pidPassed && len(config.GlobalConfig.GCPath) < 1 {
		output, err := getGCLogFile(pid)
		if err == nil && len(output) > 0 {
			config.GlobalConfig.GCPath = output
		}
	}

	fmt.Printf("PID is %d\n", pid)
	fmt.Printf("YC_SERVER is %s\n", config.GlobalConfig.Server)
	fmt.Printf("API_KEY is %s\n", config.GlobalConfig.ApiKey)
	fmt.Printf("APP_NAME is %s\n", config.GlobalConfig.AppName)
	fmt.Printf("JAVA_HOME is %s\n", config.GlobalConfig.JavaHomePath)
	fmt.Printf("GC_LOG is %s\n\n", config.GlobalConfig.GCPath)

	var err error
	defer func() {
		if err != nil {
			logger.Log("Unexpected Error %s", err)
		}
	}()
	// -------------------------------------------------------------------
	//  Create output files
	// -------------------------------------------------------------------
	timestamp := time.Now().Format("2006-01-02T15-04-05")
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)
	endpoint := fmt.Sprintf("%s/ycrash-receiver?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)

	dname := "yc-" + timestamp
	err = os.Mkdir(dname, 0777)
	if err != nil {
		return
	}
	if config.GlobalConfig.DeferDelete {
		wg.Add(1)
		defer func() {
			defer wg.Done()
			err := os.RemoveAll(dname)
			if err != nil {
				logger.Log("WARNING: Can not remove the current directory: %s", err)
				return
			}
		}()
	}
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	defer func() {
		err := os.Chdir(dir)
		if err != nil {
			logger.Log("WARNING: Can not chdir: %s", err)
			return
		}
		if config.GlobalConfig.OnlyCapture {
			err := zipFolder(dname)
			if err != nil {
				logger.Log("WARNING: Can not zip folder: %s", err)
				return
			}
		}
	}()
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
	mwriter := io.MultiWriter(fscreen, os.Stderr).(logger.Writer)
	logger.SetWriter(mwriter)
	logger.Log("yc agent version: " + shell.SCRIPT_VERSION)
	logger.Log("yc script starting...")

	// Display the PIDs which have been input to the script
	logger.Log("PROBLEMATIC_PID is: %d", pid)

	// Display the being used in this script
	logger.Log("SCRIPT_SPAN = %d", shell.SCRIPT_SPAN)
	logger.Log("JAVACORE_INTERVAL = %d", shell.JAVACORE_INTERVAL)
	logger.Log("TOP_INTERVAL = %d", shell.TOP_INTERVAL)
	logger.Log("TOP_DASH_H_INTERVAL = %d", shell.TOP_DASH_H_INTERVAL)
	logger.Log("VMSTAT_INTERVAL = %d", shell.VMSTAT_INTERVAL)

	if pidPassed && !shell.IsProcessExists(pid) {
		defer func() {
			logger.Log("WARNING: Process %d doesn't exist.", pid)
			logger.Log("WARNING: You have entered non-existent processId. Please enter valid process id")
		}()
	}

	// check if it can find gc log from ps
	var gc *os.File
	gc, err = processGCLogFile(config.GlobalConfig.GCPath, "gc.log")
	if err != nil {
		logger.Log("process log file failed %s, err: %s", config.GlobalConfig.GCPath, err.Error())
	}
	var jstat shell.CmdManager
	if pidPassed && (err != nil || gc == nil) {
		gc, jstat, err = shell.CommandStartInBackgroundToFile("gc.log",
			shell.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"})
		if err == nil {
			config.GlobalConfig.GCPath = "gc.log"
			logger.Log("gc log set to %s", config.GlobalConfig.GCPath)
		} else {
			defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
		}
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()

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

	var capNetStat *capture.NetStat
	var netStat chan capture.Result
	var capTop *capture.Top
	var top chan capture.Result
	var capVMStat *capture.VMStat
	var vmstat chan capture.Result
	var threadDump chan capture.Result
	var capPS *capture.PS
	var ps chan capture.Result
	var disk chan capture.Result
	if pidPassed {
		// ------------------------------------------------------------------------------
		//                   Capture netstat x2
		// ------------------------------------------------------------------------------
		//  Collect the first netstat: date at the top, data, and then a blank line
		logger.Log("Collecting the first netstat snapshot...")
		capNetStat = capture.NewNetStat()
		netStat = goCapture(endpoint, capture.WrapRun(capNetStat))
		logger.Log("First netstat snapshot complete.")

		// ------------------------------------------------------------------------------
		//                   Capture top
		// ------------------------------------------------------------------------------
		//  It runs in the background so that other tasks can be completed while this runs.
		logger.Log("Starting collection of top data...")
		capTop = &capture.Top{}
		top = goCapture(endpoint, capture.WrapRun(capTop))
		logger.Log("Collection of top data started.")

		// ------------------------------------------------------------------------------
		//                   Capture vmstat
		// ------------------------------------------------------------------------------
		//  It runs in the background so that other tasks can be completed while this runs.
		logger.Log("Starting collection of vmstat data...")
		capVMStat = &capture.VMStat{}
		vmstat = goCapture(endpoint, capture.WrapRun(capVMStat))
		logger.Log("Collection of vmstat data started.")

		//  Initialize some loop variables
		m := shell.SCRIPT_SPAN / shell.JAVACORE_INTERVAL
		capPS = capture.NewPS()
		ps = goCapture(endpoint, capture.WrapRun(capPS))
		logger.Log("Collecting ps snapshot...")
		for n := 1; n <= m; n++ {
			capPS.Continue()
		}
		logger.Log("Collected ps snapshot.")

		// ------------------------------------------------------------------------------
		//  				Capture dmesg
		// ------------------------------------------------------------------------------
		logger.Log("Collecting other data.  This may take a few moments...")
		// There is no endpoint for this now.
		// dmesg := goCapture(endpoint, captureDMesg)
		// ------------------------------------------------------------------------------
		//  				Capture Disk Usage
		// ------------------------------------------------------------------------------
		disk = goCapture(endpoint, capture.WrapRun(&capture.Disk{}))

		logger.Log("Collected other data.")
	}

	// ------------------------------------------------------------------------------
	//   				Capture thread dumps
	// ------------------------------------------------------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid:      pid,
		TdPath:   config.GlobalConfig.ThreadDumpPath,
		JavaHome: config.GlobalConfig.JavaHomePath,
	}
	threadDump = goCapture(endpoint, capture.WrapRun(capThreadDump))

	// ------------------------------------------------------------------------------
	//                Capture final netstat
	// ------------------------------------------------------------------------------
	if capNetStat != nil {
		logger.Log("Collecting the final netstat snapshot...")
		capNetStat.Done()
		logger.Log("Final netstat snapshot complete.")
	}

	var ok bool
	var msg string

	if jstat != nil {
		jstat.Wait()
	}
	// stop started tasks
	if capTop != nil {
		capTop.Kill()
	}
	if capVMStat != nil {
		capVMStat.Kill()
	}
	if capPS != nil {
		capPS.Kill()
	}

	// -------------------------------
	//     Transmit Top data
	// -------------------------------
	if top != nil {
		result := <-top
		fmt.Printf(
			`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit DF data
	// -------------------------------
	if disk != nil {
		result := <-disk
		fmt.Printf(
			`DISK USAGE DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit netstat data
	// -------------------------------
	if netStat != nil {
		result := <-netStat
		fmt.Printf(
			`NETSTAT DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit ps data
	// -------------------------------
	if ps != nil {
		result := <-ps
		fmt.Printf(
			`PROCESS STATUS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit VMstat data
	// -------------------------------
	if vmstat != nil {
		result := <-vmstat
		fmt.Printf(
			`VMstat DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok = shell.PostData(endpoint, "gc", gc)
	absGCPath, err := filepath.Abs(config.GlobalConfig.GCPath)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", config.GlobalConfig.GCPath, err.Error())
	}
	fmt.Printf(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absGCPath, ok, msg)

	// -------------------------------
	//     Transmit Thread dump
	// -------------------------------
	if threadDump != nil {
		result := <-threadDump
		fmt.Printf(
			`THREAD DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit MetaInfo
	// -------------------------------
	msg, ok, err = writeMetaInfo(pid, config.GlobalConfig.AppName, endpoint)
	if err != nil {
		msg = fmt.Sprintf("capture meta info failed: %s", err.Error())
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
	ep := fmt.Sprintf("%s/yc-receiver-heap?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)
	hd := config.GlobalConfig.M3 || config.GlobalConfig.HeapDump
	capHeapDump := capture.NewHeapDump(config.GlobalConfig.JavaHomePath, pid, config.GlobalConfig.HeapDumpPath, hd)
	capHeapDump.SetEndpoint(ep)
	hdResult, err := capHeapDump.Run()
	if err != nil {
		hdResult.Msg = fmt.Sprintf("capture heap dump failed: %s", err.Error())
		err = nil
	}
	fmt.Printf(
		`HEAP DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, hdResult.Ok, hdResult.Msg)

	// ------------------------------------------------------------------------------
	//  				Execute custom commands
	// ------------------------------------------------------------------------------
	logger.Log("Executing custom commands")
	for i, command := range config.GlobalConfig.Commands {
		customCmd := capture.Custom{
			Index:     i,
			UrlParams: string(command.UrlParams),
			Command:   cmdline.Split(string(command.Cmd)),
		}
		customCmd.SetEndpoint(endpoint)
		result, err := customCmd.Run()
		if err != nil {
			logger.Log("WARNING: Failed to execute custom command %d:%s, cause: %s", i, command.Cmd, err.Error())
			continue
		}
		fmt.Printf(
			`CUSTOM CMD %d: %s
Is transmission completed: %t
Resp: %s

--------------------------------
`, i, command.Cmd, result.Ok, result.Msg)
	}
	logger.Log("Executed custom commands")

	// -------------------------------
	//     Conclusion
	// -------------------------------
	finEp := fmt.Sprintf("%s/yc-fin?apiKey=%s&%s", config.GlobalConfig.Server, config.GlobalConfig.ApiKey, parameters)
	requestFin(finEp)

	ou := strings.SplitN(config.GlobalConfig.ApiKey, "@", 2)[0]
	reportEndpoint := fmt.Sprintf("%s/yc-report.jsp?ou=%s&%s", config.GlobalConfig.Server, ou, parameters)
	sfmt.Printf(`
See the report: %s
--------------------------------
`, reportEndpoint)
}

func requestFin(endpoint string) (resp []byte, err error) {
	if config.GlobalConfig.OnlyCapture {
		err = errors.New("in only capture mode")
		return
	}
	post, err := http.Post(endpoint, "text", nil)
	if err == nil {
		defer post.Body.Close()
		resp, err = ioutil.ReadAll(post.Body)
		if err == nil {
			sfmt.Printf(
				`yc-fin endpoint: %s
Resp: %s

--------------------------------
`, endpoint, resp)
		}
	}
	if err != nil {
		logger.Log("post yc-fin err %s", err.Error())
	}
	return
}

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
	// config.GlobalConfig.GCPath exists, cp it
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

	// config.GlobalConfig.GCPath is not exists, maybe using -XX:+UseGCLogFileRotation
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
	msg, ok = shell.PostData(endpoint, "meta", file)
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
	result.Msg, result.Ok = shell.PostData(endpoint, "dmesg", dmesg)
	return
}
