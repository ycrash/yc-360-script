package m3

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"shell/internal/agent/common"
	"shell/internal/agent/ondemand"
	"shell/internal/capture"
	"shell/internal/capture/executils"
	"shell/internal/config"
	"shell/internal/logger"

	"github.com/bmatcuk/doublestar/v4"
)

type M3App struct {
	runLock  sync.Mutex
	appLogM3 *capture.AppLogM3
}

func NewM3App() *M3App {
	appLogM3 := capture.NewAppLogM3()

	return &M3App{
		appLogM3: appLogM3,
	}
}

func (m3 *M3App) RunLoop() {
	initialRun := true

	for {
		if initialRun {
			initialRun = false
		} else {
			time.Sleep(config.GlobalConfig.M3Frequency)
		}

		m3.RunSingle()
	}
}

func (m3 *M3App) RunSingle() error {
	m3.runLock.Lock()
	defer m3.runLock.Unlock()

	now, timezone := common.GetAgentCurrentTime()
	timestamp := now.Format("2006-01-02T15-04-05")

	pids, err := m3.processM3(timestamp, GetM3ReceiverEndpoint(timestamp, timezone))

	if err != nil {
		logger.Log("WARNING: processM3 failed, %s", err)
		return err
	}

	finEndpoint := GetM3FinEndpoint(timestamp, timezone, pids)
	resp, err := ondemand.RequestFin(finEndpoint)

	if err != nil {
		logger.Log("WARNING: Request M3 Fin failed, %s", err)
		return err
	}

	if len(resp) <= 0 {
		logger.Log("WARNING: skip empty resp")
		return err
	}

	err = processM3FinResponse(resp, pids)

	if err != nil {
		logger.Log("WARNING: processResp failed, %s", err)
		return err
	}

	return nil
}

func GetM3ReceiverEndpoint(timestamp string, timezone string) string {
	return fmt.Sprintf("%s/m3-receiver?%s", config.GlobalConfig.Server, GetM3CommonEndpointParameters(timestamp, timezone))
}

func GetM3FinEndpoint(timestamp string, timezone string, pids map[int]string) string {
	parameters := GetM3CommonEndpointParameters(timestamp, timezone)

	if len(pids) > 0 {
		var ps, ns strings.Builder
		i := 0
		for pid, name := range pids {
			ps.WriteString(strconv.Itoa(pid))
			ns.WriteString(name)
			i++
			if i == len(pids) {
				break
			}
			ps.WriteString("($)")
			ns.WriteString("($)")
		}
		parameters += "&pids=" + ps.String() + "&m3apptoken=" + ns.String()
	}

	parameters += "&cpuCount=" + strconv.Itoa(runtime.NumCPU())

	return fmt.Sprintf("%s/m3-fin?%s", config.GlobalConfig.Server, parameters)
}

func GetM3CommonEndpointParameters(timestamp string, timezone string) string {
	// Get the server's local time zone
	parameters := fmt.Sprintf("de=%s&ts=%s", capture.GetOutboundIP().String(), timestamp)

	// Encode the server's time zone as base64
	timezoneBase64 := base64.StdEncoding.EncodeToString([]byte(timezone))
	parameters += "&timezoneID=" + timezoneBase64

	return parameters
}

func (m3 *M3App) processM3(timestamp string, endpoint string) (pids map[int]string, err error) {
	directoryName := "yc-" + timestamp
	err = os.Mkdir(directoryName, 0777)
	if err != nil {
		return
	}

	// Cleanup directory
	defer func() {
		err := os.RemoveAll(directoryName)
		if err != nil {
			logger.Log("WARNING: Can not remove the current directory: %s", err)
			return
		}
	}()

	dir, err := os.Getwd()
	if err != nil {
		return
	}

	// Reset Chdir
	defer os.Chdir(dir)

	// @Andy: This prevents concurrent uses
	// Could be eliminated to prevent issues
	err = os.Chdir(directoryName)
	if err != nil {
		return
	}

	logger.Log("yc agent version: " + executils.SCRIPT_VERSION)
	logger.Log("yc script starting in m3 mode...")

	logger.Log("Starting collection of top data...")
	capTop := &capture.Top4M3{}
	top := capture.GoCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")

	// @Andy: If this is m3 specific, it could be moved to m3 specific file for clarity
	pids, err = capture.GetProcessIds(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)

	if err == nil && len(pids) > 0 {
		// @Andy: Existing code does this synchronously. Why not async like on-demand?
		for pid, appName := range pids {
			logger.Log("uploading gc log for pid %d", pid)
			gcPath := uploadGCLogM3(endpoint, pid)

			logger.Log("uploading thread dump for pid %d", pid)
			uploadThreadDumpM3(endpoint, pid, true)

			logger.Log("Starting collection of app logs data...")
			m3.uploadAppLogM3(endpoint, pid, appName, gcPath)
		}
	} else {
		if err != nil {
			logger.Log("WARNING: failed to get PID cause %v", err)
		} else {
			logger.Log("WARNING: No PID includes ProcessTokens(%v) without ExcludeTokens(%v)",
				config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
		}
	}

	// Wait for the result of async captures
	if top != nil {
		result := <-top
		logger.Log(
			`TOP DATA
Is transmission completed: %t
Resp: %s

--------------------------------
`, result.Ok, result.Msg)
	}

	return
}

func uploadGCLogM3(endpoint string, pid int) string {
	var gcPath string
	bs, err := ondemand.RunGCCaptureCmd(pid)
	dockerID, _ := capture.GetDockerID(pid)
	if err == nil && len(bs) > 0 {
		gcPath = string(bs)
	} else {
		output, err := ondemand.GetGCLogFile(pid)
		if err == nil && len(output) > 0 {
			gcPath = output
		}
	}
	var gc *os.File
	fn := fmt.Sprintf("gc.%d.log", pid)
	gc, err = capture.ProcessGCLogFile(gcPath, fn, dockerID, pid)
	if err != nil {
		logger.Log("process log file failed %s, err: %s", gcPath, err.Error())
	}
	var jstat executils.CmdManager
	var triedJAttachGC bool
	if gc == nil || err != nil {
		gc, jstat, err = executils.CommandStartInBackgroundToFile(fn,
			executils.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"}, executils.SudoHooker{PID: pid})
		if err == nil {
			gcPath = fn
			logger.Log("gc log set to %s", gcPath)
		} else {
			triedJAttachGC = true
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach...", err.Error())
			gc, jstat, err = captureGC(pid, gc, fn)
			if err == nil {
				gcPath = fn
				logger.Log("jattach gc log set to %s", gcPath)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
		}
	}
	defer func() {
		if gc != nil {
			gc.Close()
		}
	}()
	if jstat != nil {
		err := jstat.Wait()
		if err != nil && !triedJAttachGC {
			logger.Log("jstat failed cause %s, Trying to capture gc log using jattach...", err.Error())
			gc, jstat, err = captureGC(pid, gc, fn)
			if err == nil {
				gcPath = fn
				logger.Log("jattach gc log set to %s", gcPath)
			} else {
				defer logger.Log("WARNING: no -gcPath is passed and failed to capture gc log: %s", err.Error())
			}
			err = jstat.Wait()
			if err != nil {
				logger.Log("jattach gc log failed cause %s", err.Error())
			}
		}
	}

	// -------------------------------
	//     Transmit GC Log
	// -------------------------------
	msg, ok := capture.PostCustomDataWithPositionFunc(endpoint, fmt.Sprintf("dt=gc&pid=%d", pid), gc, capture.PositionLast5000Lines)
	absGCPath, err := filepath.Abs(gcPath)
	if err != nil {
		absGCPath = fmt.Sprintf("path %s: %s", gcPath, err.Error())
	}
	logger.Log(
		`GC LOG DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absGCPath, ok, msg)

	return gcPath
}

func captureGC(pid int, gc *os.File, fn string) (file *os.File, jstat executils.CmdManager, err error) {
	if gc != nil {
		err = gc.Close()
		if err != nil {
			return
		}
		err = os.Remove(fn)
		if err != nil {
			return
		}
	}
	// file deepcode ignore CommandInjection: security vulnerability
	file, jstat, err = executils.CommandStartInBackgroundToFile(fn,
		executils.Command{executils.Executable(), "-p", strconv.Itoa(pid), "-gcCaptureMode"}, executils.EnvHooker{"pid": strconv.Itoa(pid)}, executils.SudoHooker{PID: pid})
	return
}

func uploadThreadDumpM3(endpoint string, pid int, sendPidParam bool) {
	var threadDump chan capture.Result
	gcPath := config.GlobalConfig.GCPath
	tdPath := config.GlobalConfig.ThreadDumpPath
	hdPath := config.GlobalConfig.HeapDumpPath
	ondemand.UpdatePaths(pid, &gcPath, &tdPath, &hdPath)

	// endpoint, pid, tdPath
	// ------------------------------------------------------------------------------
	//   				Capture thread dumps
	// ------------------------------------------------------------------------------
	capThreadDump := &capture.ThreadDump{
		Pid:      pid,
		TdPath:   tdPath,
		JavaHome: config.GlobalConfig.JavaHomePath,
	}
	if sendPidParam {
		capThreadDump.SetEndpointParam("pid", strconv.Itoa(pid))
	}
	threadDump = capture.GoCapture(endpoint, capture.WrapRun(capThreadDump))
	// -------------------------------
	//     Log Thread dump
	// -------------------------------
	absTDPath, err := filepath.Abs(tdPath)
	if err != nil {
		absTDPath = fmt.Sprintf("path %s: %s", tdPath, err.Error())
	}
	if threadDump != nil {
		result := <-threadDump
		logger.Log(
			`THREAD DUMP DATA
%s
Is transmission completed: %t
Resp: %s

--------------------------------
`, absTDPath, result.Ok, result.Msg)
	}
}

func (m3 *M3App) uploadAppLogM3(endpoint string, pid int, appName string, gcPath string) {
	var appLogM3Chan chan capture.Result

	useGlobalConfigAppLogs := false
	if len(config.GlobalConfig.AppLogs) > 0 {
		appLogs := config.AppLogs{}

		for _, configAppLog := range config.GlobalConfig.AppLogs {
			searchToken := "$" + appName

			beforeSearchToken, found := strings.CutSuffix(string(configAppLog), searchToken)
			if found {
				appLogs = append(appLogs, config.AppLog(beforeSearchToken))
			}

		}

		if len(appLogs) > 0 {
			paths := make(map[int]config.AppLogs)
			paths[pid] = appLogs

			appLogM3 := m3.appLogM3
			appLogM3.SetPaths(paths)

			useGlobalConfigAppLogs = true
			appLogM3Chan = capture.GoCapture(endpoint, capture.WrapRun(appLogM3))
		}
	}

	if !useGlobalConfigAppLogs {
		// Auto discover app logs
		discoveredLogFiles, err := capture.DiscoverOpenedLogFilesByProcess(pid)
		if err != nil {
			logger.Log("Error on auto discovering app logs: %s", err.Error())
		}

		// To exclude GC log files from app logs discovery
		pattern := capture.GetGlobPatternFromGCPath(gcPath, pid)
		globFiles, globErr := doublestar.FilepathGlob(pattern, doublestar.WithFilesOnly(), doublestar.WithNoFollow())
		if globErr != nil {
			logger.Log("App logs Auto discovery: Error on creating Glob pattern %s", pattern)
		}

		appLogs := config.AppLogs{}
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
				appLogs = append(appLogs, config.AppLog(f))
			}
		}

		appLogM3 := m3.appLogM3

		paths := make(map[int]config.AppLogs)
		paths[pid] = appLogs

		appLogM3.SetPaths(paths)

		appLogM3Chan = capture.GoCapture(endpoint, capture.WrapRun(appLogM3))
	}

	logger.Log("Collection of app logs data started.")

	if appLogM3Chan != nil {
		result := <-appLogM3Chan
		logger.Log(
			`APPLOGS DATA
Ok (at least one success): %t
Resps: %s

--------------------------------
`, result.Ok, result.Msg)
	}
}

func processM3FinResponse(resp []byte, pid2Name map[int]string) (err error) {
	pids, tags, timestamps, err := ParseM3FinResponse(resp)
	if err != nil {
		logger.Log("WARNING: Get PID from ParseJsonResp failed, %s", err)
		return
	}
	t := strings.Join(tags, ",")

	tmp := config.GlobalConfig.Tags
	if len(tmp) > 0 {
		ts := strings.Trim(tmp, ",")
		tmp = strings.Trim(ts+","+t, ",")
	} else {
		tmp = strings.Trim(t, ",")
	}
	_, err = ondemand.ProcessPidsWithoutLock(pids, pid2Name, config.GlobalConfig.HeapDump, tmp, timestamps)
	return
}

type M3FinResponse struct {
	Actions    []string
	Tags       []string
	Timestamp  string
	Timestamps []string
}

func ParseM3FinResponse(resp []byte) (pids []int, tags []string, timestamps []string, err error) {
	// Init empty slice instead of []int(nil)
	pids = []int{}
	tags = []string{}
	timestamps = []string{}

	r := &M3FinResponse{}
	err = json.Unmarshal(resp, r)
	if err != nil {
		return
	}

	tags = r.Tags
	if len(r.Timestamps) > 0 {
		// If the new "timestamps" field is present
		timestamps = r.Timestamps
	} else if r.Timestamp != "" {
		// If the new "timestamps" is not present,
		// Use the legacy "timestamp" field
		timestamps = append(timestamps, r.Timestamp)
	}

	for _, s := range r.Actions {
		if strings.HasPrefix(s, "capture ") {
			ss := strings.Split(s, " ")
			if len(ss) == 2 {
				id := ss[1]
				pid, err := strconv.Atoi(id)
				if err != nil {
					continue
				}
				pids = append(pids, pid)
			}
		}
	}
	return
}
