package capture

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const (
	dotnetGCDefaultDirName    = "yc-dot-net-gc"
	dotnetGCTempUploadLogName = "gc.log"
)

// gcReadState represents the state of a GC artifact read attempt.
type gcReadState int

const (
	gcStateMissing gcReadState = iota
	gcStateEmpty
	gcStateValid
	gcStateError
)

func (s gcReadState) String() string {
	switch s {
	case gcStateMissing:
		return "missing"
	case gcStateEmpty:
		return "empty"
	case gcStateValid:
		return "valid"
	case gcStateError:
		return "error"
	default:
		return "unknown"
	}
}

type DotnetGCSession struct {
	PID            int
	ProcessStartTs int64
	Cmd            executils.CmdManager
	LogPath        string
	StartedAt      time.Time
	LastErr        error
}

// DotnetGCAsync manages asynchronous .NET GC collectors in M3 mode.
//
// High-level relationship with M3App.RunSingle:
//   - In each RunSingle cycle, M3App.captureAndTransmit() calls Reconcile(activeDotnetPIDs).
//   - Reconcile() starts/stops/restarts long-running yc-dot-net GC collectors per PID.
//   - Later in the same cycle, M3App.uploadDotnetGCM3() calls UploadFromSession()
//     to read that session's JSON artifact and upload it to m3-receiver (dt=gc&pid=<pid>).
//
// This split keeps collector lifecycle management here, while M3 controls per-cycle upload timing.
type DotnetGCAsync struct {
	mu       sync.Mutex
	sessions map[int]*DotnetGCSession
	baseDir  string
}

// NewDotnetGCAsync creates a new async .NET GC manager with a resolved base directory.
func NewDotnetGCAsync(baseDir string) *DotnetGCAsync {
	resolvedDir, err := resolveDotnetGCBaseDir(baseDir)
	if err != nil {
		logger.Log("WARNING: failed to resolve dotnet gc base dir (%s), fallback to current dir", err)
		resolvedDir = "."
	}

	return &DotnetGCAsync{
		sessions: make(map[int]*DotnetGCSession),
		baseDir:  resolvedDir,
	}
}

// Reconcile aligns collector sessions with currently active .NET PIDs.
func (d *DotnetGCAsync) Reconcile(active map[int]string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for pid := range d.sessions {
		if _, ok := active[pid]; !ok {
			_ = d.stopLocked(pid, "pid removed from process scan")
		}
	}

	for pid, appName := range active {
		sess, ok := d.sessions[pid]
		if !ok || sess == nil {
			if err := d.ensureStartedLocked(pid, appName); err != nil {
				logger.Log("WARNING: failed starting dotnet gc collector pid=%d app=%s: %s", pid, appName, err)
			}
			continue
		}

		if d.isSessionAliveLocked(sess) && d.isSameProcessIdentityLocked(sess) {
			continue
		}

		logger.Log("WARN dotnet gc collector exited unexpectedly pid=%d; retrying on this reconcile", sess.PID)

		// Example scenario: the original target process exits, then the OS reuses the same PID
		// for a different process. In that case identity/liveness checks fail, so we must restart
		// the collector and bind it to the current process behind this PID.
		if err := d.restartLocked(pid, appName, "collector dead or process identity mismatch"); err != nil {
			sess.LastErr = err
			logger.Log("WARNING: dotnet gc collector restart failed pid=%d app=%s: %s", pid, appName, err)
		}
	}
}

// EnsureStarted starts or validates a collector session for a PID.
func (d *DotnetGCAsync) EnsureStarted(pid int, appName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ensureStartedLocked(pid, appName)
}

// ensureStartedLocked starts a collector for PID if no valid live session exists.
func (d *DotnetGCAsync) ensureStartedLocked(pid int, appName string) error {
	if pid <= 0 {
		return fmt.Errorf("invalid pid %d", pid)
	}

	if sess, ok := d.sessions[pid]; ok && sess != nil {
		if d.isSessionAliveLocked(sess) && d.isSameProcessIdentityLocked(sess) {
			return nil
		}
		_ = d.stopLocked(pid, "ensure started requested restart")
	}

	if err := ensureDir(d.baseDir); err != nil {
		return fmt.Errorf("failed creating dotnet gc dir %s: %w", d.baseDir, err)
	}

	startTs, err := GetProcessStartTimestamp(pid)
	if err != nil {
		return fmt.Errorf("failed getting process start timestamp for pid=%d: %w", pid, err)
	}

	args := []string{"-gc", strconv.Itoa(pid), d.baseDir, "-1"}
	cmd, err := startDotnetToolInBackground(args)
	if err != nil {
		return err
	}

	logPath := filepath.Join(d.baseDir, fmt.Sprintf(dotnetGCOutputPath, pid))
	d.sessions[pid] = &DotnetGCSession{
		PID:            pid,
		ProcessStartTs: startTs,
		Cmd:            cmd,
		LogPath:        logPath,
		StartedAt:      time.Now(),
	}

	logger.Log("started async dotnet gc collector pid=%d app=%s output=%s", pid, appName, logPath)
	return nil
}

// IsRunning reports whether the session for PID is alive and still bound to same process identity.
func (d *DotnetGCAsync) IsRunning(pid int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	sess, ok := d.sessions[pid]
	if !ok || sess == nil {
		return false
	}

	return d.isSessionAliveLocked(sess) && d.isSameProcessIdentityLocked(sess)
}

// LogPath returns the collector output path for PID when a session exists.
func (d *DotnetGCAsync) LogPath(pid int) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	sess, ok := d.sessions[pid]
	if !ok || sess == nil {
		return "", false
	}

	return sess.LogPath, true
}

// Stop terminates a collector session for PID.
func (d *DotnetGCAsync) Stop(pid int, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopLocked(pid, reason)
}

// StopAll terminates all active collector sessions.
func (d *DotnetGCAsync) StopAll(reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for pid := range d.sessions {
		_ = d.stopLocked(pid, reason)
	}
}

// stopLocked removes and terminates a collector session for PID under mutex protection.
func (d *DotnetGCAsync) stopLocked(pid int, reason string) error {
	sess, ok := d.sessions[pid]
	if !ok || sess == nil {
		return nil
	}

	delete(d.sessions, pid)

	if sess.Cmd == nil {
		return nil
	}

	if err := sess.Cmd.KillAndWait(); err != nil {
		logger.Log("WARNING: failed stopping dotnet gc collector pid=%d reason=%s err=%s", pid, reason, err)
		return err
	}

	logger.Log("stopped async dotnet gc collector pid=%d reason=%s", pid, reason)
	return nil
}

// restartLocked performs stop-then-start for PID under mutex protection.
func (d *DotnetGCAsync) restartLocked(pid int, appName, reason string) error {
	_ = d.stopLocked(pid, reason)
	return d.ensureStartedLocked(pid, appName)
}

// UploadFromSession reads a JSON artifact from session output and uploads it.
func (d *DotnetGCAsync) UploadFromSession(endpoint string, pid int, suppressStartupWarnings bool) (Result, bool) {
	logPath, ok := d.LogPath(pid)
	if !ok {
		return Result{Msg: fmt.Sprintf("dotnet gc session not found pid=%d", pid), Ok: false}, false
	}

	payload, state, err := d.readDotnetGCJSON(logPath)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed reading dotnet gc artifact pid=%d path=%s err=%s", pid, logPath, err), Ok: false}, false
	}

	if payload == nil {
		switch state {
		case gcStateMissing, gcStateEmpty:
			if suppressStartupWarnings {
				logger.Debug().Msgf("dotnet gc artifact not ready yet pid=%d path=%s state=%v", pid, logPath, state)
			} else {
				logger.Log("WARNING: dotnet gc artifact unavailable pid=%d path=%s state=%v", pid, logPath, state)
			}
		default:
			logger.Log("WARNING: invalid dotnet gc JSON pid=%d path=%s", pid, logPath)
		}

		return Result{Msg: fmt.Sprintf("dotnet gc payload unavailable state=%v path=%s", state, logPath), Ok: false}, false
	}

	gcLogFile, err := os.Create(dotnetGCTempUploadLogName)
	if err != nil {
		return Result{Msg: fmt.Sprintf("failed creating %s file pid=%d: %s", dotnetGCTempUploadLogName, pid, err), Ok: false}, false
	}
	defer gcLogFile.Close()

	if _, err = gcLogFile.Write(payload); err != nil {
		return Result{Msg: fmt.Sprintf("failed writing %s payload pid=%d: %s", dotnetGCTempUploadLogName, pid, err), Ok: false}, false
	}

	if _, err = gcLogFile.Seek(0, 0); err != nil {
		return Result{Msg: fmt.Sprintf("failed rewinding %s payload pid=%d: %s", dotnetGCTempUploadLogName, pid, err), Ok: false}, false
	}

	msg, uploaded := PostCustomData(endpoint, fmt.Sprintf("dt=gc&pid=%d", pid), gcLogFile)
	return Result{Msg: msg, Ok: uploaded}, uploaded
}

// isSessionAliveLocked checks whether both collector process and target process still exist.
func (d *DotnetGCAsync) isSessionAliveLocked(sess *DotnetGCSession) bool {
	if sess == nil || sess.Cmd == nil {
		return false
	}

	// collectorPID is the PID of yc-dot-net.exe spawned for this session.
	collectorPID := sess.Cmd.GetPid()
	if collectorPID <= 0 {
		return false
	}

	// Verify the collector process is still alive. We intentionally probe process start timestamp
	// as an existence check: if lookup fails, collector process is gone/unreachable.
	if _, err := GetProcessStartTimestamp(collectorPID); err != nil {
		return false
	}

	if _, err := GetProcessStartTimestamp(sess.PID); err != nil {
		return false
	}

	return true
}

// isSameProcessIdentityLocked ensures target PID has not been recycled to another process.
func (d *DotnetGCAsync) isSameProcessIdentityLocked(sess *DotnetGCSession) bool {
	if sess == nil {
		return false
	}

	ts, err := GetProcessStartTimestamp(sess.PID)
	if err != nil {
		return false
	}

	return ts == sess.ProcessStartTs
}

// resolveDotnetGCBaseDir returns absolute collector output base directory.
func resolveDotnetGCBaseDir(baseDir string) (string, error) {
	if baseDir != "" {
		return filepath.Abs(baseDir)
	}

	workDir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}

	return filepath.Join(workDir, dotnetGCDefaultDirName), nil
}

// ensureDir ensures directory path exists.
func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}

// readDotnetGCJSON reads artifact with short retries until non-empty payload is available.
func (d *DotnetGCAsync) readDotnetGCJSON(gcPath string) ([]byte, gcReadState, error) {
	delays := []time.Duration{0, 100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond}
	state := gcStateMissing

	for idx, delay := range delays {
		if idx > 0 {
			time.Sleep(delay)
		}

		payload, err := os.ReadFile(gcPath)
		if err != nil {
			if os.IsNotExist(err) {
				state = gcStateMissing
				continue
			}
			return nil, gcStateError, err
		}

		if len(bytes.TrimSpace(payload)) == 0 {
			state = gcStateEmpty
			continue
		}

		return payload, gcStateValid, nil
	}

	return nil, state, nil
}
