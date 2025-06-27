package m3

import (
	"context"
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

	"yc-agent/internal/agent/common"
	"yc-agent/internal/agent/ondemand"
	"yc-agent/internal/capture"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/bmatcuk/doublestar/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type M3App struct {
	runLock     sync.Mutex
	appLogM3    *capture.AppLogM3
	accessLogM3 *capture.AccessLogM3
}

func NewM3App() *M3App {
	appLogM3 := capture.NewAppLogM3()
	accessLogM3 := capture.NewAccessLogM3()

	return &M3App{
		appLogM3:    appLogM3,
		accessLogM3: accessLogM3,
	}
}

func (m3 *M3App) RunLoop() {
	for {
		m3.RunSingle()
		time.Sleep(config.GlobalConfig.M3Frequency.Duration())
	}
}

func (m3 *M3App) RunSingle() error {
	logger.Debug().Msgf("M3App.RunSingle: running M3 capture")

	m3.runLock.Lock()
	defer m3.runLock.Unlock()

	now, timezone := common.GetAgentCurrentTime()
	timestamp := now.Format("2006-01-02T15-04-05")

	pids, err := capture.GetProcessIds(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	logger.Debug().Msgf("M3App.RunSingle: got process IDs: %v", pids)

	if err != nil {
		logger.Log("WARNING: failed to get PID cause %v", err)
	} else if len(pids) == 0 {
		logger.Log("WARNING: No PID includes ProcessTokens(%v) without ExcludeTokens(%v)",
			config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)
	}

	// Init directory
	// TODO: This has a similar functionality with ondemand. It might be good to extract this to a common reusable function.
	{
		captureDir := "yc-" + timestamp
		if len(config.GlobalConfig.StoragePath) > 0 {
			captureDir = filepath.Join(config.GlobalConfig.StoragePath, captureDir)
		}

		logger.Debug().Msgf("M3App.RunSingle: about to create and change directory to %s", captureDir)

		{
			err = os.Mkdir(captureDir, 0777)
			if err != nil {
				return err
			}

			// Cleanup directory
			if config.GlobalConfig.DeferDelete {
				defer func() {
					err := os.RemoveAll(captureDir)
					if err != nil {
						logger.Log("WARNING: Can not remove the current directory: %s", err)
						return
					}
				}()
			}
		}

		{
			dir, err := os.Getwd()
			if err != nil {
				return err
			}

			// @Andy: This prevents concurrent uses
			// Could be eliminated to prevent issues
			err = os.Chdir(captureDir)
			if err != nil {
				return err
			}

			// Reset Chdir
			defer os.Chdir(dir)
		}

		logger.Debug().Msgf("M3App.RunSingle: successfully changed directory to %s", captureDir)
	}

	// Capture
	{
		logger.Debug().Msgf("M3App.RunSingle: about to call captureAndTransmit")

		err = m3.captureAndTransmit(pids, GetM3ReceiverEndpoint(timestamp, timezone))
		if err != nil {
			logger.Log("WARNING: processM3 failed, %s", err)
			return err
		}
	}

	// Finish
	{
		logger.Debug().Msgf("M3App.RunSingle: about to call fin endpoint")

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

	/// append pod name and namespace in Kubernetes
	if config.GlobalConfig.Kubernetes {
		podName := getPodName()
		//parameters += "&pod=" + podName
		ns := getMatchingNamespace(podName)
		if ns != "" {
			//	parameters += "&ns=" + ns
			parameters += "&pod=" + ns + "_" + podName
			logger.Log("Namespace -> %s", ns)
			logger.Log("Podname -> %s", podName)
			logger.Log("OS -> %s", runtime.GOOS)
			logger.Log("Architecture -> %s", runtime.GOARCH)
			logger.Log("CPUs -> %d", runtime.NumCPU())
		}
	}
	///
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

func (m3 *M3App) captureAndTransmit(pids map[int]string, endpoint string) (err error) {
	logger.Log("yc-360 script version: " + executils.SCRIPT_VERSION)
	logger.Log("yc-360 script starting in m3 mode...")

	logger.Log("Starting collection of top data...")
	capTop := &capture.Top4M3{}
	top := capture.GoCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")

	if len(pids) > 0 {
		// @Andy: Existing code does this synchronously. Why not async like on-demand?
		for pid, appName := range pids {
			logger.Log("uploading gc log for pid %d", pid)
			gcPath := uploadGCLogM3(endpoint, pid)

			logger.Log("uploading thread dump for pid %d", pid)
			uploadThreadDumpM3(endpoint, pid, true)

			logger.Log("Starting collection of app logs data...")
			m3.uploadAppLogM3(endpoint, pid, appName, gcPath)

			logger.Log("Starting collection of access logs data...")
			m3.uploadAccessLogM3(endpoint, pid, appName)

			if healthCheckCfg, ok := config.GlobalConfig.HealthChecks[appName]; ok {
				uploadHealthCheck(endpoint, appName, healthCheckCfg)
			}
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
	capThreadDump.SetEndpointParam("cpuCount", strconv.Itoa(runtime.NumCPU()))

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

		appLogsContainDollarSign := false
		for _, configAppLog := range config.GlobalConfig.AppLogs {
			if strings.Contains(string(configAppLog), "$") {
				appLogsContainDollarSign = true
				break
			}
		}

		if appLogsContainDollarSign {
			for _, configAppLog := range config.GlobalConfig.AppLogs {
				searchToken := "$" + appName

				beforeSearchToken, found := strings.CutSuffix(string(configAppLog), searchToken)
				if found {
					appLogs = append(appLogs, config.AppLog(beforeSearchToken))
				}
			}
		} else {
			appLogs = config.GlobalConfig.AppLogs
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
			logger.Log("Error on auto discovering app logs -> %s", err.Error())
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
Ok (at least one transmitted): %t
Resps: %s

--------------------------------
`, result.Ok, result.Msg)
	}
}

func (m3 *M3App) uploadAccessLogM3(endpoint string, pid int, appName string) {
	var accessLogM3Chan chan capture.Result
	if len(config.GlobalConfig.AccessLogs) <= 0 {
		return
	}

	accessLogEntries := []capture.AccessLogEntry{}

	accessLogPath := ""
	accessLogFormat := ""

	searchToken := "$" + appName

	// Find the corresponding access log path
	for _, configAccessLog := range config.GlobalConfig.AccessLogs {
		if !strings.Contains(string(configAccessLog), "$") {
			continue
		}

		beforeSearchToken, found := strings.CutSuffix(string(configAccessLog), searchToken)
		if found {
			accessLogPath = beforeSearchToken
		}
	}

	// Find the corresponding access log format
	for _, configAccessLogFmt := range config.GlobalConfig.AccessLogFormats {
		if !strings.Contains(string(configAccessLogFmt), "$") {
			continue
		}

		beforeSearchToken, found := strings.CutSuffix(string(configAccessLogFmt), searchToken)
		if found {
			accessLogFormat = beforeSearchToken
		}
	}

	if accessLogPath == "" {
		return
	}

	if accessLogFormat == "" {
		logger.Log("WARNING: Access log format is not set for path:%s, app:%s, ignoring", accessLogPath, appName)
		return
	}

	accessLogEntries = append(accessLogEntries, capture.AccessLogEntry{
		Path:   config.AccessLogPath(accessLogPath),
		Format: config.AccessLogFormat(accessLogFormat),
	})

	paths := make(map[int][]capture.AccessLogEntry)
	paths[pid] = accessLogEntries

	accessLogM3 := m3.accessLogM3
	accessLogM3.SetPaths(paths)

	// Run the capture asynchronously
	accessLogM3Chan = capture.GoCapture(endpoint, capture.WrapRun(accessLogM3))
	logger.Log("Collection of access logs data started.")

	// Wait for the result
	if accessLogM3Chan != nil {
		result := <-accessLogM3Chan
		logger.Log(
			`ACCESSLOGS DATA
Ok (at least one transmitted): %t
Resps: %s

--------------------------------
`, result.Ok, result.Msg)
	}
}

func uploadHealthCheck(endpoint, appName string, healthCheckCfg config.HealthCheck) {
	capHealthCheck := &capture.HealthCheck{
		AppName: appName,
		Cfg:     healthCheckCfg,
	}
	chanHealthCheck := capture.GoCapture(endpoint, capture.WrapRun(capHealthCheck))

	if chanHealthCheck != nil {
		result := <-chanHealthCheck
		logger.Log(
			`HEALTH CHECK DATA
Is transmission completed: %t
Resp: %s

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
	_, err = ondemand.ProcessPids(pids, pid2Name, config.GlobalConfig.HeapDump, tmp, timestamps)
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

// /
// / When YC Agent is deployed as a side car, then the hostname is podname
func getPodName() string {
	var podName = ""
	if config.GlobalConfig.Kubernetes {
		hostname, e := os.Hostname()
		if e != nil {
			logger.Log("Error while getting hostname -> %s", e.Error())
		}
		podName = hostname
		// logger.Log("Podname: %s", podName)
	}
	return podName
}

// // get the matching namespace based on the pod name passed
// // implemented for Trinet in Dec,2024
func getMatchingNamespace(podName string) string {
	config, err := rest.InClusterConfig()
	if err != nil {
		logger.Log("Error while configuring cluster -> %s", err.Error())
		return ""
	}

	// Create the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logger.Log("Error while creating new config for cluster -> %s", err.Error())
		return ""
	}

	// get all the namespaces
	namespaces, err := clientset.CoreV1().Namespaces().List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		logger.Log("Error listing namespaces -> %v", err)
	}

	// iterate through all the namespaces
	for _, ns := range namespaces.Items {
		// logger.Log("namespaces -> %s", ns)
		// pass the podname to get a proper match, if error nil means the passed podname
		// exists in the collection of pods for that namepace
		// then, return that namespace
		_, err := clientset.CoreV1().Pods(ns.Name).Get(context.TODO(), podName, metav1.GetOptions{})
		if err == nil {
			// fmt.Printf("Pod '%s' found in namespace: %s\n", podName, ns.Name)
			return ns.Name
		}
	}
	return ""
}
