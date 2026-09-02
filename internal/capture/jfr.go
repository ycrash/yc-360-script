package capture

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

const jfrFileName = "my.jfr"

const (
	jfrDefaultDuration = 60 * time.Second
	jfrMinDuration     = 10 * time.Second
	jfrMaxDuration     = 5 * time.Minute

	// jfrDumpGrace is how long we wait past the end of the recording window
	// before opening the file. The JVM's duration= timer stops the recording
	// and writes it out on its own, and nothing tells us when that write
	// finished, so this is the margin that covers it.
	jfrDumpGrace = 5 * time.Second

	// jfrThreadDumpPeriod is how often the recording samples a full thread
	// dump (the jdk.ThreadDump event) via JFR itself, independent of the
	// separate jstack-based thread dump capture elsewhere in this package.
	jfrThreadDumpPeriod = 10 * time.Second
)

// jfrFailurePhrases are substrings JFR/jcmd print in a diagnostic command's
// response text on failure, even though the jcmd/jattach process itself
// exits 0 (it successfully delivered the command; the JVM just refused it).
// Mirrors the bytes.Contains checks HeapDump.heapDump does for the same
// reason: a 0 exit code alone doesn't mean the JFR command succeeded.
var jfrFailurePhrases = []string{
	"could not start recording",
	"is already",      // e.g. "Recording with name 'X' is already running/being used"
	"no such process", // target pid vanished between IsProcessExists and jcmd
}

// JFR captures a JVM Flight Recorder recording for a running Java process.
// The recording is started with duration=, so the JVM stops it and writes it
// out on its own after Duration - the agent never sends JFR.stop. The JVM
// writes to the temp directory (jfrRecordingPath); yc then stages the result
// into the capture directory as jfrFileName. FullCapture reads its result last, so
// the recording window overlaps the rest of the capture instead of adding to
// it.
type JFR struct {
	Capture
	Pid      int
	JavaHome string
	// Duration is how long the recording runs. Zero means jfrDefaultDuration.
	Duration time.Duration
}

func (t *JFR) Run() (Result, error) {
	file, err := t.CaptureToFile()
	if err != nil {
		return Result{Msg: err.Error(), Ok: false}, err
	}
	defer file.Close()

	return t.UploadCapturedFile(file), nil
}

// CaptureToFile starts a JFR recording that runs for the configured duration,
// waits for the JVM's own timer to stop it and write it out, then opens the
// resulting recording file.
func (t *JFR) CaptureToFile() (*os.File, error) {
	if !IsProcessExists(t.Pid) {
		return nil, fmt.Errorf("process %d does not exist", t.Pid)
	}

	// Unique per invocation (pid + timestamp), not a fixed constant: back to
	// back runs against the same long-lived JVM (e.g. repeated manual
	// testing) must never collide with a still-active recording left behind
	// by a previous run.
	nanos := time.Now().UnixNano()
	name := fmt.Sprintf("ycJFR-%d-%d", t.Pid, nanos)

	duration := t.effectiveDuration()

	jvmPath := jfrRecordingPath(t.Pid, nanos)
	if err := t.startRecording(name, jvmPath, duration); err != nil {
		return nil, err
	}

	// DeferDelete only cleans the capture directory, so this file is ours to
	// remove. Reassigned below if the recording is in the target's namespace.
	sourcePath := jvmPath
	defer func() {
		if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) {
			logger.Log("WARNING: could not remove the JVM's JFR recording %s: %v", sourcePath, err)
		}
	}()

	wait := duration + jfrDumpGrace
	logger.Log("JFR recording %s started, running for %s; reading it in %s", name, duration, wait)
	time.Sleep(wait)

	resolved, err := resolveRecordingPath(t.Pid, jvmPath)
	if err != nil {
		return nil, err
	}
	sourcePath = resolved

	return stageRecording(sourcePath, jfrFileName)
}

func (t *JFR) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(t.Endpoint(), "jfr", file)
	return Result{Msg: msg, Ok: ok}
}

// effectiveDuration is the recording window to use: t.Duration clamped to
// [jfrMinDuration, jfrMaxDuration], or jfrDefaultDuration when it isn't set.
func (t *JFR) effectiveDuration() time.Duration {
	switch {
	case t.Duration == 0:
		return jfrDefaultDuration
	case t.Duration < 0:
		logger.Warn().Msgf("jfrCaptureDuration %s is negative; using the default %s", t.Duration, jfrDefaultDuration)
		return jfrDefaultDuration
	case t.Duration < jfrMinDuration:
		logger.Warn().Msgf("jfrCaptureDuration %s is below the %s minimum; using %s", t.Duration, jfrMinDuration, jfrMinDuration)
		return jfrMinDuration
	case t.Duration > jfrMaxDuration:
		logger.Warn().Msgf("jfrCaptureDuration %s is above the %s maximum; using %s", t.Duration, jfrMaxDuration, jfrMaxDuration)
		return jfrMaxDuration
	default:
		return t.Duration
	}
}

func jfrArg(key, value string) string {
	return fmt.Sprintf("%s=%q", key, value)
}

func jfrPathUsable(p string) bool {
	return !strings.ContainsAny(p, "\"'\n\r")
}

// jfrTimespan renders d the way JFR's argument parser wants it: a number
// followed by a single unit.
func jfrTimespan(d time.Duration) string {
	return fmt.Sprintf("%ds", int(d.Round(time.Second).Seconds()))
}

// jfrRecordingPath is where the target JVM writes its recording: a unique name
// in the system temp directory, deliberately *not* the capture directory.
func jfrRecordingPath(pid int, nanos int64) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s.%d.%d", jfrFileName, pid, nanos))
}

// startRecording starts a JFR recording named name on the target JVM, writing
// to jvmPath and running for duration.
//
// duration= puts the timer inside the JVM: it stops the recording and writes
// the file with nobody attached, so the agent needs only this one diagnostic
// command, and a recording can't outlive the agent if yc is killed partway
// through.
func (t *JFR) startRecording(name, jvmPath string, duration time.Duration) error {
	if !jfrPathUsable(jvmPath) {
		return fmt.Errorf("JFR recording path %q contains a quote or newline, which jcmd can't express", jvmPath)
	}

	// duration= and the jdk.ThreadDump#period= event setting need no quoting:
	// jfrTimespan can only produce digits and 's'. settings=profile is a
	// bare identifier (one of the JDK's built-in .jfc names), so it needs no
	// quoting either.
	cmd := fmt.Sprintf("JFR.start %s %s duration=%s settings=profile jdk.ThreadDump#period=%s",
		jfrArg("name", name), jfrArg("filename", jvmPath), jfrTimespan(duration), jfrTimespan(jfrThreadDumpPeriod))
	if _, err := t.runJcmd(cmd); err != nil {
		return fmt.Errorf("failed to start JFR recording: %w", err)
	}

	return nil
}

func resolveRecordingPath(pid int, jvmPath string) (string, error) {
	if _, err := os.Stat(jvmPath); err == nil {
		return jvmPath, nil
	}

	if runtime.GOOS != "linux" {
		return "", fmt.Errorf("the JVM did not write a JFR recording to %s", jvmPath)
	}

	nsPath := filepath.Join("/proc", strconv.Itoa(pid), "root", jvmPath)
	if _, err := os.Stat(nsPath); err != nil {
		return "", fmt.Errorf("no JFR recording at %s, nor in the target's mount namespace at %s", jvmPath, nsPath)
	}

	logger.Log("JFR recording not visible at %s; reading it from the target's mount namespace at %s", jvmPath, nsPath)
	return nsPath, nil
}

func stageRecording(src, dst string) (*os.File, error) {
	if err := os.Rename(src, dst); err != nil {
		if err := copyRecording(src, dst); err != nil {
			return nil, err
		}
	}

	file, err := os.Open(dst)
	if err != nil {
		return nil, fmt.Errorf("failed to open staged JFR recording %s: %w", dst, err)
	}

	if info, err := file.Stat(); err == nil {
		logger.Log("staged JFR recording as %s (%d bytes)", dst, info.Size())
	}

	return file, nil
}

func copyRecording(src, dst string) error {
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open JFR recording %s: %w", src, err)
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create JFR recording %s: %w", dst, err)
	}
	defer dstFile.Close()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		return fmt.Errorf("failed to copy JFR recording %s to %s: %w", src, dst, err)
	}

	if err := dstFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync JFR recording %s: %w", dst, err)
	}

	return nil
}

// runJcmd runs a jcmd diagnostic command against t.Pid, trying the JDK's
// jcmd binary first and falling back to jattach, then tmp jattach (mirrors
// HDSub.executeJcmd). Every attempt goes through the timeout-bounded
// executils.CommandCombinedOutputToWriter (config.GlobalConfig.CmdTimeout),
// so a hung jcmd/jattach process can't block the capture indefinitely. It
// returns the command's response text on success.
//
// A 0 exit code only means jcmd/jattach successfully delivered the command;
// it says nothing about whether the JVM actually honored it (e.g. JFR.start
// prints an error and exits 0 if a recording by that name/id can't be
// started). jcmdSucceeded checks the response text for that.
func (t *JFR) runJcmd(command string) (string, error) {
	var out bytes.Buffer

	err := executils.CommandCombinedOutputToWriter(&out,
		executils.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), command},
		executils.SudoHooker{PID: t.Pid})
	if err == nil && jcmdSucceeded(out.String()) {
		logger.Log("jcmd %s: %s", command, out.String())
		return out.String(), nil
	}
	logger.Log("jcmd failed (err=%v): %s. Falling back to jattach for: %s", err, out.String(), command)
	out.Reset()

	err = executils.CommandCombinedOutputToWriter(&out,
		executils.Command{executils.Executable(), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(t.Pid)}, executils.SudoHooker{PID: t.Pid})
	if err == nil && jcmdSucceeded(out.String()) {
		logger.Log("jattach %s: %s", command, out.String())
		return out.String(), nil
	}
	logger.Log("jattach failed (err=%v): %s. Falling back to tmp jattach for: %s", err, out.String(), command)
	firstAttemptOutput := out.String()
	out.Reset()

	tempPath, tmpErr := executils.Copy2TempPath()
	if tmpErr != nil {
		return "", fmt.Errorf("failed to run %q: %s (tmp jattach fallback failed: %v)", command, firstAttemptOutput, tmpErr)
	}

	err = executils.CommandCombinedOutputToWriter(&out,
		executils.Command{tempPath, "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(t.Pid)}, executils.SudoHooker{PID: t.Pid})
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %w, output: %s", command, err, out.String())
	}
	if !jcmdSucceeded(out.String()) {
		return "", fmt.Errorf("failed to run %q: JVM rejected the command: %s", command, out.String())
	}
	logger.Log("tmp jattach %s: %s", command, out.String())
	return out.String(), nil
}

// jcmdSucceeded reports whether a jcmd/jattach response indicates the JVM
// actually honored the diagnostic command, as opposed to rejecting it while
// the wrapping process still exits 0.
func jcmdSucceeded(output string) bool {
	lower := strings.ToLower(output)
	for _, phrase := range jfrFailurePhrases {
		if strings.Contains(lower, phrase) {
			return false
		}
	}
	return true
}
