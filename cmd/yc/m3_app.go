package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"shell/internal"
	"shell/internal/capture"
	"shell/internal/config"
	"shell/internal/logger"
	"strconv"
	"strings"
	"sync"
	"time"

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

	timestamp := time.Now().Format("2006-01-02T15-04-05")

	pids, err := m3.processM3(timestamp, GetM3ReceiverEndpoint(timestamp))

	if err != nil {
		logger.Log("WARNING: processM3 failed, %s", err)
		return err
	}

	finEndpoint := GetM3FinEndpoint(timestamp, pids)
	resp, err := requestM3Fin(finEndpoint)

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

func GetM3ReceiverEndpoint(timestamp string) string {
	return fmt.Sprintf("%s/m3-receiver?%s", config.GlobalConfig.Server, GetM3CommonEndpointParameters(timestamp))
}

func GetM3FinEndpoint(timestamp string, pids map[int]string) string {
	parameters := GetM3CommonEndpointParameters(timestamp)

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

func GetM3CommonEndpointParameters(timestamp string) string {
	// Get the server's local time zone
	serverTimeZone := getServerTimeZone()
	parameters := fmt.Sprintf("de=%s&ts=%s", getOutboundIP().String(), timestamp)

	// Encode the server's time zone as base64
	timezoneBase64 := base64.StdEncoding.EncodeToString([]byte(serverTimeZone))
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

	logger.Log("yc agent version: " + internal.SCRIPT_VERSION)
	logger.Log("yc script starting in m3 mode...")

	logger.Log("Starting collection of top data...")
	capTop := &capture.Top4M3{}
	top := goCapture(endpoint, capture.WrapRun(capTop))
	logger.Log("Collection of top data started.")

	// @Andy: If this is m3 specific, it could be moved to m3 specific file for clarity
	pids, err = internal.GetProcessIds(config.GlobalConfig.ProcessTokens, config.GlobalConfig.ExcludeProcessTokens)

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
	bs, err := runGCCaptureCmd(pid)
	dockerID, _ := internal.GetDockerID(pid)
	if err == nil && len(bs) > 0 {
		gcPath = string(bs)
	} else {
		output, err := getGCLogFile(pid)
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
	var jstat internal.CmdManager
	var triedJAttachGC bool
	if gc == nil || err != nil {
		gc, jstat, err = internal.CommandStartInBackgroundToFile(fn,
			internal.Command{path.Join(config.GlobalConfig.JavaHomePath, "/bin/jstat"), "-gc", "-t", strconv.Itoa(pid), "2000", "30"}, internal.SudoHooker{PID: pid})
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
	msg, ok := internal.PostCustomDataWithPositionFunc(endpoint, fmt.Sprintf("dt=gc&pid=%d", pid), gc, internal.PositionLast5000Lines)
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

func uploadThreadDumpM3(endpoint string, pid int, sendPidParam bool) {
	var threadDump chan capture.Result
	gcPath := config.GlobalConfig.GCPath
	tdPath := config.GlobalConfig.ThreadDumpPath
	hdPath := config.GlobalConfig.HeapDumpPath
	updatePaths(pid, &gcPath, &tdPath, &hdPath)

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
	threadDump = goCapture(endpoint, capture.WrapRun(capThreadDump))
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

			beforeSearchToken, found := cutSuffix(string(configAppLog), searchToken)
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
			appLogM3Chan = goCapture(endpoint, capture.WrapRun(appLogM3))
		}
	}

	if !useGlobalConfigAppLogs {
		// Auto discover app logs
		discoveredLogFiles, err := DiscoverOpenedLogFilesByProcess(pid)
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

		appLogM3Chan = goCapture(endpoint, capture.WrapRun(appLogM3))
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

func requestM3Fin(endpoint string) (resp []byte, err error) {
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

func processM3FinResponse(resp []byte, pid2Name map[int]string) (err error) {
	pids, tags, timestamps, err := internal.ParseM3FinResponse(resp)
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
	_, err = processPidsWithoutLock(pids, pid2Name, config.GlobalConfig.HeapDump, tmp, timestamps)
	return
}

// CutSuffix returns s without the provided ending suffix string
// and reports whether it found the suffix.
// If s doesn't end with suffix, CutSuffix returns s, false.
// If suffix is the empty string, CutSuffix returns s, true.
// This is a shim for strings.CutPrefix. Once we upgraded go version to a recent one,
// this should be replaced with strings.CutPrefix.
func cutSuffix(s, suffix string) (before string, found bool) {
	if !strings.HasSuffix(s, suffix) {
		return s, false
	}
	return s[:len(s)-len(suffix)], true
}
