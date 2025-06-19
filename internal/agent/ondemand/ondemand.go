package ondemand

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"yc-agent/internal/agent/common"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/capture/java"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/gentlemanautomaton/cmdline"
	"github.com/pterm/pterm"
	ps "github.com/shirou/gopsutil/v3/process"
)

var Wg sync.WaitGroup

func ProcessPids(pids []int, pid2Name map[int]string, hd bool, tags string, timestamps []string) (rUrls []string, err error) {
	if len(pids) <= 0 {
		logger.Log("Empty pids, no action needed.")
		return
	}

	pids = removeDuplicate(pids)

	for i, pid := range pids {
		name := config.GlobalConfig.AppName
		if len(pid2Name) > 0 {
			if n, ok := pid2Name[pid]; ok {
				name = n
			}
		}

		if len(config.GlobalConfig.CaptureCmd) > 0 {
			_, err := executils.RunCaptureCmd(pid, config.GlobalConfig.CaptureCmd)
			if err != nil {
				logger.Log("WARNING: runCaptureCmd failed %s", err)
				continue
			}
		} else {
			var timestamp string
			// In case pids has more elements than timetamps,
			// the extra elements will use "" timestamp
			// which will be defaulted to now in fullProcess().
			if i > len(timestamps)-1 {
				timestamp = ""
			} else {
				timestamp = timestamps[i]
			}

			url := FullCapture(pid, name, hd, tags, timestamp)
			if len(url) > 0 {
				rUrls = append(rUrls, url)
			}
		}
	}

	return
}

func FullCapture(pid int, appName string, hd bool, tags string, tsParam string) (rUrl string) {
	var err error
	defer func() {
		if err != nil {
			logger.Error().Err(err).Msg("unexpected error")
		}
	}()

	// -------------------------------------------------------------------
	// A. Init
	// -------------------------------------------------------------------
	var endpoint string
	var parameters string

	{
		var timestamp string

		// A.1 Define yc-server endpoint and parameters
		{
			now, timezone := common.GetAgentCurrentTime()
			timestamp = now.Format("2006-01-02T15-04-05")

			if tsParam == "" {
				tsParam = timestamp
			}
			//parameters = fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), tsParam)
			timezoneBase64 := base64.StdEncoding.EncodeToString([]byte(timezone))
			parameters = fmt.Sprintf("de=%s&ts=%s&timezoneId=%s", getOutboundIP().String(), tsParam, timezoneBase64)
			endpoint = fmt.Sprintf("%s/ycrash-receiver?%s", config.GlobalConfig.Server, parameters)
		}

		// A.2 Setup capture directory: yc-$timestamp
		// TODO: This has a similar functionality with m3. It might be good to extract this to a common reusable function.
		{
			captureDir := "yc-" + timestamp
			if len(config.GlobalConfig.StoragePath) > 0 {
				captureDir = filepath.Join(config.GlobalConfig.StoragePath, captureDir)
			}

			{
				if !config.GlobalConfig.M3 {
					err = os.Mkdir(captureDir, 0777)
					if err != nil {
						return
					}
				}
				// Cleanup capture dir
				if config.GlobalConfig.DeferDelete {
					Wg.Add(1)
					defer func() {
						defer Wg.Done()
						err := os.RemoveAll(captureDir)
						if err != nil {
							logger.Log("WARNING: Can not remove the current directory: %s", err)
							return
						}
					}()
				}
			}

			{
				// Store current dir for later
				dir, err := os.Getwd()
				if err != nil {
					return
				}

				// Chdir to the capture dir (yc-$timestamp)
				if !config.GlobalConfig.M3 {
					err = os.Chdir(captureDir)
					if err != nil {
						return
					}
				}

				defer func() {
					// Chdir to the original dir
					err := os.Chdir(dir)
					if err != nil {
						logger.Log("WARNING: Can not chdir: %s", err)
						return
					}
					if config.GlobalConfig.OnlyCapture {
						name, err := ZipFolder(captureDir)
						if err != nil {
							logger.Log("WARNING: Can not zip folder: %s", err)
							return
						}
						logger.StdLog("All dumps can be found in %s", name)
						if logger.Log2File {
							logger.Log("All dumps can be found in %s", name)
						}
					}
				}()
			}
		}
	}

	// A.3 Agent log file
	var agentLogFile *os.File
	if !config.GlobalConfig.M3 {
		agentLogFile, err = logger.StartWritingToFile("agentlog.out")
		if err != nil {
			logger.Info().Err(err).Msg("Failed to start writing to file")
		}

		defer func() {
			if agentLogFile == nil {
				return
			}
			err := logger.StopWritingToFile()
			if err != nil {
				logger.Info().Err(err).Msg("Failed to stop writing to file")
			}
		}()
	}

	// A.4 MetaInfo
	{
		msg, ok, err := writeMetaInfo(pid, appName, endpoint, tags)
		logger.Log(
			`META INFO DATA
Is transmission completed: %t
Resp: %s
Ignored errors: %v

--------------------------------
`, ok, msg, err)
	}

	if pid > 0 && !capture.IsProcessExists(pid) {
		defer func() {
			logger.Log("WARNING: Process %d doesn't exist.", pid)
			logger.Log("WARNING: You have entered non-existent processId. Please enter valid process id")
		}()
	}

	// -------------------------------------------------------------------
	// B. Capture and Transmit
	// -------------------------------------------------------------------

	startTime := time.Now()
	gcPath := config.GlobalConfig.GCPath
	tdPath := config.GlobalConfig.ThreadDumpPath
	hdPath := config.GlobalConfig.HeapDumpPath
	UpdatePaths(pid, &gcPath, &tdPath, &hdPath)
	pidPassed := pid > 0

	var dockerID string
	if pidPassed {
		// find gc log path in from command line arguments of ps result
		if len(gcPath) == 0 {
			output, err := GetGCLogFile(pid)
			if err == nil && len(output) > 0 {
				gcPath = output
			}
		}

		dockerID, _ = capture.GetDockerID(pid)
	}

	// B.1 Log capture configs
	{
		logger.Log("PID is %d", pid)
		logger.Log("YC_SERVER is %s", config.GlobalConfig.Server)
		logger.Log("API_KEY is %s", config.GlobalConfig.ApiKey)
		logger.Log("APP_NAME is %s", appName)
		logger.Log("JAVA_HOME is %s", config.GlobalConfig.JavaHomePath)
		logger.Log("GC_LOG is %s", gcPath)
		if len(dockerID) > 0 {
			logger.Log("DOCKER_ID is %s", dockerID)
		}

		// Display the PIDs which have been input to the script
		logger.Log("PROBLEMATIC_PID is: %d", pid)

		// Display the being used in this script
		logger.Log("SCRIPT_SPAN = %d", executils.SCRIPT_SPAN)
		logger.Log("JAVACORE_INTERVAL = %d", executils.JAVACORE_INTERVAL)
		logger.Log("TOP_INTERVAL = %d", executils.TOP_INTERVAL)
		logger.Log("TOP_DASH_H_INTERVAL = %d", executils.TOP_DASH_H_INTERVAL)
		logger.Log("VMSTAT_INTERVAL = %d", executils.VMSTAT_INTERVAL)
	}

	{
		////// capture boomi details
		boomi := config.GlobalConfig.Boomi
		if boomi {
			now, _ := common.GetAgentCurrentTime()
			timestamp := now.Format("2006-01-02T15-04-05")
			logger.Log("CAPTURING BOOMI DETAILS..%s->", config.GlobalConfig.BoomiUrl)
			capture.CaptureBoomiDetails(endpoint, timestamp, pid)
		}
	}

	// ------------------------------------------------------------------------------
	//   				Capture gc
	// ------------------------------------------------------------------------------
	gc := goCapture(endpoint, capture.WrapRun(&capture.GC{
		Pid:      pid,
		JavaHome: config.GlobalConfig.JavaHomePath,
		DockerID: dockerID,
		GCPath:   gcPath,
	}))
	var capNetStat *capture.NetStat
	var netStat chan capture.Result
	var capTop *capture.Top
	var top chan capture.Result
	var capVMStat *capture.VMStat
	var vmstat chan capture.Result
	var dmesg chan capture.Result
	var threadDump chan capture.Result
	var capPS *capture.PS
	var ps chan capture.Result
	var disk chan capture.Result
	if pidPassed {
		// ------------------------------------------------------------------------------
		//                   Capture netstat x2
		// ------------------------------------------------------------------------------
		//  Collect the first netstat: date at the top, data, and then a blank line
		capNetStat = &capture.NetStat{}
		netStat = goCapture(endpoint, capture.WrapRun(capNetStat))

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

		logger.Log("Collecting ps snapshot...")
		capPS = capture.NewPS()
		ps = goCapture(endpoint, capture.WrapRun(capPS))
		logger.Log("Collected ps snapshot.")

		// ------------------------------------------------------------------------------
		//  				Capture dmesg
		// ------------------------------------------------------------------------------
		logger.Log("Collecting other data.  This may take a few moments...")
		dmesg = goCapture(endpoint, capture.WrapRun(&capture.DMesg{}), capVMStat)
		// ------------------------------------------------------------------------------
		//  				Capture Disk Usage
		// ------------------------------------------------------------------------------
		disk = goCapture(endpoint, capture.WrapRun(&capture.Disk{}))

		logger.Log("Collected other data.")
	}

	// ------------------------------------------------------------------------------
	//   				Capture ping
	// ------------------------------------------------------------------------------
	ping := goCapture(endpoint, capture.WrapRun(&capture.Ping{Host: config.GlobalConfig.PingHost}))

	// ------------------------------------------------------------------------------
	//   				Capture kernel params
	// ------------------------------------------------------------------------------
	kernel := goCapture(endpoint, capture.WrapRun(&capture.Kernel{}))

	// ------------------------------------------------------------------------------
	//   				Capture thread dumps
	// ------------------------------------------------------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid:               pid,
		TdPath:            tdPath,
		JavaHome:          config.GlobalConfig.JavaHomePath,
		TdCaptureDuration: config.GlobalConfig.TDCaptureDuration,
	}
	threadDump = goCapture(endpoint, capture.WrapRun(capThreadDump))

	// ------------------------------------------------------------------------------
	//   				Capture legacy app log
	// ------------------------------------------------------------------------------
	var appLog chan capture.Result
	if len(config.GlobalConfig.AppLog) > 0 && config.GlobalConfig.AppLogLineCount != 0 {
		configAppLogs := config.AppLogs{config.AppLog(config.GlobalConfig.AppLog)}
		appLog = goCapture(endpoint, capture.WrapRun(&capture.AppLog{Paths: configAppLogs, LineLimit: config.GlobalConfig.AppLogLineCount}))
	}

	// ------------------------------------------------------------------------------
	//   				Capture app logs
	// ------------------------------------------------------------------------------
	var appLogs chan capture.Result
	useGlobalConfigAppLogs := false
	if len(config.GlobalConfig.AppLogs) > 0 && config.GlobalConfig.AppLogLineCount != 0 {

		appLogsContainDollarSign := false
		for _, configAppLog := range config.GlobalConfig.AppLogs {
			if strings.Contains(string(configAppLog), "$") {
				appLogsContainDollarSign = true
				break
			}
		}

		if appLogsContainDollarSign {
			// If any of the appLogs contain '$', choose only the matched appName
			appLogsMatchingAppName := config.AppLogs{}

			for _, configAppLog := range config.GlobalConfig.AppLogs {
				searchToken := "$" + appName

				beforeSearchToken, found := strings.CutSuffix(string(configAppLog), searchToken)
				if found {
					appLogsMatchingAppName = append(appLogsMatchingAppName, config.AppLog(beforeSearchToken))
				}

			}

			if len(appLogsMatchingAppName) > 0 {
				appLogs = goCapture(endpoint, capture.WrapRun(&capture.AppLog{Paths: appLogsMatchingAppName, LineLimit: config.GlobalConfig.AppLogLineCount}))
				useGlobalConfigAppLogs = true
			}
		} else {
			appLogs = goCapture(endpoint, capture.WrapRun(&capture.AppLog{Paths: config.GlobalConfig.AppLogs, LineLimit: config.GlobalConfig.AppLogLineCount}))
			useGlobalConfigAppLogs = true
		}
	}

	if !useGlobalConfigAppLogs {
		// Auto discover app logs
		discoveredLogFiles, err := capture.DiscoverOpenedLogFilesByProcess(pid)
		if err != nil {
			logger.Log("Error on auto discovering app logs: %s", err.Error())
		}

		// To exclude GC log files from app logs discovery
		globFiles := []string{}

		// Need to check gcPath not empty. Otherwise, empty pattern will return an unexpected result: [".", "."]
		if gcPath != "" {
			pattern := capture.GetGlobPatternFromGCPath(gcPath, pid)

			var globErr error
			globFiles, globErr = doublestar.FilepathGlob(pattern, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
			if globErr != nil {
				logger.Log("App logs Auto discovery: Error on creating Glob pattern %s", pattern)
			}
		}

		paths := config.AppLogs{}
		for _, f := range discoveredLogFiles {
			isGCLog := false
			for _, fileName := range globFiles {
				// To exclude discovered gc log such f as /tmp/buggyapp-%p-%t.log
				// also exclude discovered gc log with rotation where such f as /tmp/buggyapp-%p-%t.log.0
				// Where the `pattern` = /tmp/buggyapp-*-*.log
				if strings.Contains(f, filepath.FromSlash(fileName)) {
					isGCLog = true
					logger.Log("App logs Auto discovery: Ignored %s because it is detected as a GC log", f)
					break
				}
			}

			if !isGCLog {
				paths = append(paths, config.AppLog(f))
			}
		}

		appLogs = goCapture(endpoint, capture.WrapRun(&capture.AppLog{Paths: paths, LineLimit: config.GlobalConfig.AppLogLineCount}))
	}

	// ------------------------------------------------------------------------------
	//   				Capture hdsub log
	// ------------------------------------------------------------------------------
	hdsubLog := goCapture(endpoint, capture.WrapRun(&capture.HDSub{
		Pid:      pid,
		JavaHome: config.GlobalConfig.JavaHomePath,
	}))

	// ------------------------------------------------------------------------------
	//   				Capture Extended Data
	// ------------------------------------------------------------------------------
	var extendedData chan capture.Result
	if config.GlobalConfig.EdScript != "" && config.GlobalConfig.EdDataFolder != "" {
		extendedData = goCapture(endpoint, capture.WrapRun(&capture.ExtendedData{Script: config.GlobalConfig.EdScript, DataFolder: config.GlobalConfig.EdDataFolder}))
	}

	// stop started tasks
	if capTop != nil {
		capTop.Kill()
	}
	if capVMStat != nil {
		capVMStat.Kill()
	}

	// -------------------------------
	//     Transmit Top data
	// -------------------------------
	if top != nil {
		logger.Log("Reading result from top channel")
		result := <-top
		logger.Log(
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
		logger.Log("Reading result from disk channel")
		result := <-disk
		logger.Log(
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
		logger.Log("Reading result from netStat channel")
		result := <-netStat
		logger.Log(
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
		logger.Log("Reading result from ps channel")
		result := <-ps
		logger.Log(
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
		logger.Log("Reading result from vmstat channel")
		result := <-vmstat
		logger.Log(
			`VMstat DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit DMesg data
	// -------------------------------
	if dmesg != nil {
		logger.Log("Reading result from dmesg channel")
		result := <-dmesg
		logger.Log(
			`DMesg DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	if gc != nil {
		logger.Log("Reading result from gc channel")
		result := <-gc
		logger.Log(
			`GC LOG DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
		if !result.Ok {
			defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log")
		}
	}

	// -------------------------------
	//     Transmit ping dump
	// -------------------------------
	if ping != nil {
		logger.Log("Reading result from ping channel")
		result := <-ping
		logger.Log(
			`PING DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit app log
	// -------------------------------
	if appLog != nil {
		logger.Log("Reading result from appLog channel")
		result := <-appLog
		logger.Log(
			`APPLOG DATA
Is transmission completed: %t
Resp:
%s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit app logs
	// -------------------------------
	if appLogs != nil {
		logger.Log("Reading result from appLogs channel")
		result := <-appLogs
		logger.Log(
			`APPLOGS DATA
Ok (at least one transmitted): %t
Resps:
%s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit hdsub log
	// -------------------------------
	if hdsubLog != nil {
		logger.Log("Reading result from hdsubLog channel")
		result := <-hdsubLog
		logger.Log(
			`HDSUB DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit kernel param dump
	// -------------------------------
	if kernel != nil {
		logger.Log("Reading result from kernel channel")
		result := <-kernel
		logger.Log(
			`KERNEL PARAMS DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit Thread dump
	// -------------------------------
	absTDPath, err := filepath.Abs(tdPath)
	if err != nil {
		absTDPath = fmt.Sprintf("path %s: %s", tdPath, err.Error())
	}
	if threadDump != nil {
		logger.Log("Reading result from threadDump channel")
		result := <-threadDump
		logger.Log(
			`THREAD DUMP DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absTDPath, result.Ok, result.Msg)
	}

	// -------------------------------
	//     Transmit Heap dump result
	// -------------------------------
	ep := fmt.Sprintf("%s/yc-receiver-heap?%s", config.GlobalConfig.Server, parameters)
	capHeapDump := capture.NewHeapDump(config.GlobalConfig.JavaHomePath, pid, hdPath, hd)
	capHeapDump.SetEndpoint(ep)
	hdResult, err := capHeapDump.Run()
	if err != nil {
		hdResult.Msg = fmt.Sprintf("capture heap dump failed: %s", err.Error())
		err = nil
	}
	logger.Log(
		`HEAP DUMP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, hdResult.Ok, hdResult.Msg)

	// -------------------------------
	//     Transmit Extended Data
	// -------------------------------
	if extendedData != nil {
		logger.Log("Reading result from extended data channel")
		result := <-extendedData
		logger.Log(
			`EXTENDED DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

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
		logger.Log(
			`CUSTOM CMD %d: %s
Is transmission completed: %t
Resp: %s

--------------------------------
`, i, command.Cmd, result.Ok, result.Msg)
	}
	logger.Log("Executed custom commands")

	if config.GlobalConfig.OnlyCapture {
		return
	}

	// -------------------------------------------------------------------
	// C. Finishing
	// -------------------------------------------------------------------

	// C.1 /yc-fin
	{
		finEp := fmt.Sprintf("%s/yc-fin?%s", config.GlobalConfig.Server, parameters)
		resp, err := RequestFin(finEp)
		if err != nil {
			logger.Log("post yc-fin err %s", err.Error())
			err = nil
		}

		endTime := time.Now()
		var result string
		rUrl, result = printResult(true, endTime.Sub(startTime).String(), resp)

		// A big customer is relying on this stdout.
		// They probably uses it with their own script / automation.
		logger.StdLog(`
%s
`, resp)

		logger.Log(`
%s
`, resp)
		logger.Log(`
%s
`, pterm.RemoveColorFromString(result))
	}

	// C.2 Transmit agentlog
	if agentLogFile != nil {
		msg, ok := capture.PostData(endpoint, "agentlog", agentLogFile)
		err := logger.StopWritingToFile()
		if err != nil {
			logger.Info().Err(err).Msg("Failed to stop writing to file")
		}
		agentLogFile = nil
		logger.Log(
			`YC-360 SCRIPT LOG DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, ok, msg)
	}

	return
}

var getOutboundIP = capture.GetOutboundIP
var goCapture = capture.GoCapture

func GetGCLogFile(pid int) (result string, err error) {
	var cmdLine []byte
	var command executils.Command
	dynamicArg := strconv.Itoa(pid)
	if runtime.GOOS == "windows" {
		dynamicArg = fmt.Sprintf("ProcessId=%d", pid)
	}

	command, _ = executils.GC.AddDynamicArg(dynamicArg)
	cmdLine, err = executils.CommandCombinedOutput(command)

	if err != nil {
		logger.Log("GetGCLogFile: err in getting process cmdline: %s, output: %s", err.Error(), string(cmdLine))
		logger.Log("GetGCLogFile: falling back to gopsutil")

		// Try fallback with gopsutil library
		p, errFallback := ps.NewProcess(int32(pid))
		if errFallback != nil {
			logger.Log("GetGCLogFile: fallback gopsutil err in getting process: %s", errFallback.Error())
			return
		}

		cmdLineStr, errFallbackCmdline := p.Cmdline()
		if errFallbackCmdline != nil {
			logger.Log("GetGCLogFile: fallback gopsutil err in getting process cmdline: %s", errFallbackCmdline.Error())
			return
		}

		// Fallback success
		if cmdLineStr != "" {
			cmdLine = []byte(cmdLineStr)
			err = nil
		}
	}

	logFile := ExtractGCLogPathFromCmdline(string(cmdLine))

	result = strings.TrimSpace(logFile)
	if result != "" && !filepath.IsAbs(result) {
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			p, err := ps.NewProcess(int32(pid))
			if err == nil {
				cwd, err := p.Cwd()
				if err == nil {
					result = filepath.Join(cwd, result)
				}
			}
		} else {
			logger.Warn().Str("gcpath", result).Msg("Please use absolute file path for '-Xloggc' and '-Xlog:gc'")
		}
	}

	return
}

const metaInfoTemplate = `hostName=%s
processId=%d
appName=%s
whoami=%s
timestamp=%s
timezone=%s
timezoneId=%s
cpuCount=%d
javaVersion=%s
osVersion=%s
tags=%s`

func writeMetaInfo(processId int, appName, endpoint, tags string) (msg string, ok bool, err error) {
	file, err := os.Create("meta-info.txt")
	if err != nil {
		return
	}
	defer file.Close()
	hostname, e := os.Hostname()
	if e != nil {
		err = fmt.Errorf("hostname err: %v", e)
	}
	var jv string
	javaVersion, e := executils.CommandCombinedOutput(executils.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/java"), "-version"})
	if e != nil {
		err = fmt.Errorf("javaVersion err: %v, previous err: %v", e, err)
	} else {
		jv = strings.ReplaceAll(string(javaVersion), "\r\n", ", ")
		jv = strings.ReplaceAll(jv, "\n", ", ")
	}
	var ov string
	osVersion, e := executils.CommandCombinedOutput(executils.OSVersion)
	if e != nil {
		err = fmt.Errorf("osVersion err: %v, previous err: %v", e, err)
	} else {
		ov = strings.ReplaceAll(string(osVersion), "\r\n", ", ")
		ov = strings.ReplaceAll(ov, "\n", ", ")
	}
	var un string
	current, e := user.Current()
	if e != nil {
		err = fmt.Errorf("username err: %v, previous err: %v", e, err)
	} else {
		un = current.Username
	}

	now, timezoneIANA := common.GetAgentCurrentTime()
	timestamp := now.Format("2006-01-02T15-04-05")
	timezone, _ := now.Zone()
	cpuCount := runtime.NumCPU()
	_, e = file.WriteString(fmt.Sprintf(metaInfoTemplate, hostname, processId, appName, un, timestamp, timezone, timezoneIANA, cpuCount, jv, ov, tags))
	if e != nil {
		err = fmt.Errorf("write result err: %v, previous err: %v", e, err)
		return
	}
	msg, ok = capture.PostData(endpoint, "meta", file)
	return
}

func RunGCCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.GCCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = executils.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func RunTDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.TDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = executils.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func RunHDCaptureCmd(pid int) (path []byte, err error) {
	cmd := config.GlobalConfig.HDCaptureCmd
	if len(cmd) < 1 {
		return
	}
	path, err = executils.RunCaptureCmd(pid, cmd)
	if err != nil {
		return
	}
	path = bytes.TrimSpace(path)
	return
}

func UpdatePaths(pid int, gcPath, tdPath, hdPath *string) {
	var path []byte
	if len(*gcPath) == 0 {
		path, _ = RunGCCaptureCmd(pid)
		if len(path) > 0 {
			*gcPath = string(path)
		}
	}
	if len(*tdPath) == 0 {
		// Thread dump: Attempt 4: tdCaptureCmd argument (real step is 2 ), adjusting tdPath argument
		path, _ = RunTDCaptureCmd(pid)
		if len(path) > 0 {
			*tdPath = string(path)
		}
	}
	if len(*hdPath) == 0 {
		path, _ = RunHDCaptureCmd(pid)
		if len(path) > 0 {
			*hdPath = string(path)
		}
	}
}

func RequestFin(endpoint string) (resp []byte, err error) {
	if config.GlobalConfig.OnlyCapture {
		err = errors.New("in only capture mode")
		return
	}
	transport := http.DefaultTransport.(*http.Transport)
	transport.TLSClientConfig = &tls.Config{
		InsecureSkipVerify: !config.GlobalConfig.VerifySSL,
	}
	path := config.GlobalConfig.CACertPath
	if len(path) > 0 {
		pool := x509.NewCertPool()
		var ca []byte
		ca, err = ioutil.ReadFile(path)
		if err != nil {
			return
		}
		pool.AppendCertsFromPEM(ca)
		transport.TLSClientConfig.RootCAs = pool
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   config.GlobalConfig.HttpClientTimeout,
	}
	req, err := http.NewRequest("POST", endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "text")
	req.Header.Set("ApiKey", config.GlobalConfig.ApiKey)
	post, err := httpClient.Do(req)
	if err == nil {
		defer post.Body.Close()
		resp, err = ioutil.ReadAll(post.Body)
		if err == nil {
			logger.Log(
				`yc-fin endpoint: %s
Resp: %s

--------------------------------
`, endpoint, resp)
		}
	}
	return
}

func removeDuplicate[T comparable](sliceList []T) []T {
	allKeys := make(map[T]bool)
	list := []T{}
	for _, item := range sliceList {
		if _, value := allKeys[item]; !value {
			allKeys[item] = true
			list = append(list, item)
		}
	}
	return list
}

func ExtractGCLogPathFromCmdline(cmdline string) string {
	var logFile string
	cmdlineBytes := []byte(cmdline)

	if logFile == "" {
		// Garbage collection log: Attempt 1: -Xloggc:<file-path>
		re := regexp.MustCompile("-Xloggc:(\\S+)")
		matches := re.FindSubmatch(cmdlineBytes)
		if len(matches) == 2 {
			logFile = string(matches[1])
		}
	}

	if logFile == "" {
		// Garbage collection log: Attempt 2: -Xlog:gc*:file=<file-path>
		// -Xlog[:option]
		//	option         :=  [<what>][:[<output>][:[<decorators>][:<output-options>]]]
		// https://openjdk.org/jeps/158
		re := regexp.MustCompile("-Xlog:gc\\S*:file=(\\S+)")
		matches := re.FindSubmatch(cmdlineBytes)
		if len(matches) == 2 {
			logFile = string(matches[1])

			if strings.Contains(logFile, ":") {
				logFile = java.GetFileFromJEP158(logFile)
			}
		}
	}

	if logFile == "" {
		// Garbage collection log: Attempt 3: -Xlog:gc:<file-path>
		re := regexp.MustCompile("-Xlog:gc:(\\S+)")
		matches := re.FindSubmatch(cmdlineBytes)
		if len(matches) == 2 {
			logFile = string(matches[1])

			if strings.Contains(logFile, ":") {
				logFile = java.GetFileFromJEP158(logFile)
			}
		}
	}

	if logFile == "" {
		// Garbage collection log: Attempt 4: -Xverbosegclog:/tmp/buggy-app-gc-log.%pid.log,20,10
		re := regexp.MustCompile("-Xverbosegclog:(\\S+)")
		matches := re.FindSubmatch(cmdlineBytes)
		if len(matches) == 2 {
			logFile = string(matches[1])

			if strings.Contains(logFile, ",") {
				splitByComma := strings.Split(logFile, ",")
				// Check if it's in the form of filename,x,y
				// Take only filename
				if len(splitByComma) == 3 {
					logFile = splitByComma[0]
				}
			}
		}
	}
	return logFile
}
