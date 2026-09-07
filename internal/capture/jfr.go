package capture

import (
	"bytes"
	"errors"
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

	// The JVM's duration= timer stops the recording and writes it out on its
	// own, and nothing announces when that write finished. Asking the JVM is
	// the only reliable answer: once the recording window is over we poll
	// JFR.check every jfrCheckInterval, for at most jfrCheckTimeout, until
	// its listing shows the recording closed or gone. A fixed grace period
	// used to stand in for this, and on a busy machine it was routinely too
	// short - which staged the empty file JFR.start creates up front.
	jfrCheckInterval = 2 * time.Second
	jfrCheckTimeout  = 20 * time.Second
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
// out on its own after Duration - the agent never sends JFR.stop, and waits
// for JFR.check to report the recording finished before it touches the file.
// The JVM writes to the temp directory (jfrRecordingPath); yc then stages the
// result into the capture directory as jfrFileName. FullCapture reads its
// result last, so the recording window overlaps the rest of the capture
// instead of adding to it.
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
// waits for the JVM's own timer to stop it and write it out - confirmed by
// JFR.check rather than assumed from a fixed grace period - then opens the
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
	// remove - but only once the JVM is finished with it. At the end of the
	// window the JVM reopens the path it was given at JFR.start and appends
	// the recording there; it does not recreate the file, so taking the file
	// away while the recording is still running destroys the recording
	// outright. Reassigned below if the recording is in the target's namespace.
	sourcePath := jvmPath
	finished := false
	defer func() {
		if !finished {
			return
		}
		if err := os.Remove(sourcePath); err != nil && !os.IsNotExist(err) {
			logger.Log("WARNING: could not remove the JVM's JFR recording %s: %v", sourcePath, err)
		}
	}()

	logger.Log("JFR recording %s started, running for %s; then checking every %s, for up to %s, that the JVM has written it",
		name, duration, jfrCheckInterval, jfrCheckTimeout)
	time.Sleep(duration)

	if err := t.waitForRecording(name); err != nil {
		if !errors.Is(err, errJVMGone) {
			logger.Log("WARNING: leaving %s alone; the JVM may still be writing the recording there", jvmPath)
			return nil, err
		}
		// Nobody can be writing the file once the JVM is gone: an empty one
		// is the stub JFR.start created, anything else is the recording.
		finished = true
		info, statErr := os.Stat(jvmPath)
		if statErr != nil || info.Size() == 0 {
			return nil, fmt.Errorf("the target JVM exited before it wrote JFR recording %s", name)
		}
		logger.Log("the JVM exited before JFR.check could confirm recording %s; staging the %d bytes it wrote", name, info.Size())
	}
	finished = true

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

	// duration= needs no quoting: jfrTimespan can only produce digits and
	// 's'. settings=profile is a bare identifier (one of the JDK's built-in
	// .jfc names), so it needs no quoting either.
	cmd := fmt.Sprintf("JFR.start %s %s duration=%s settings=profile",
		jfrArg("name", name), jfrArg("filename", jvmPath), jfrTimespan(duration))
	if _, err := t.runJcmd(cmd); err != nil {
		return fmt.Errorf("failed to start JFR recording: %w", err)
	}

	return nil
}

// errJVMGone reports that the target process no longer exists, so no JFR.check
// can ever answer and nobody can be writing the recording.
var errJVMGone = errors.New("the target JVM is gone")

// waitForRecording blocks until JFR.check confirms the JVM has finished with
// the named recording, or jfrCheckTimeout passes without that confirmation.
// Unknown is not finished: a check that fails, or answers with something other
// than a listing, keeps polling, and if nothing confirms the recording before
// the deadline the caller leaves the file to the JVM. A check that fails
// because the JVM is gone ends the wait at once with errJVMGone.
func (t *JFR) waitForRecording(name string) error {
	return waitForRecording(name, jfrCheckTimeout, jfrCheckInterval, func() (string, error) {
		out, err := t.runJcmd("JFR.check")
		if err != nil && !IsProcessExists(t.Pid) {
			return "", errJVMGone
		}
		return out, err
	})
}

// waitForRecording is the loop behind JFR.waitForRecording, with the check
// call and the timing pluggable for tests.
func waitForRecording(name string, timeout, interval time.Duration, check func() (string, error)) error {
	deadline := time.Now().Add(timeout)
	var last string

	for {
		out, err := check()
		if errors.Is(err, errJVMGone) {
			return err
		}
		if err != nil {
			last = fmt.Sprintf("the last JFR.check failed: %v", err)
			logger.Log("WARNING: could not check on JFR recording %s: %v", name, err)
		} else if finished, listErr := jfrRecordingFinished(out, name); listErr != nil {
			last = fmt.Sprintf("the last JFR.check did not return a recording listing: %v", listErr)
			logger.Log("WARNING: JFR.check did not return a recording listing (%v): %s", listErr, strings.TrimSpace(out))
		} else if finished {
			return nil
		} else {
			last = "the last JFR.check still listed it as unfinished"
			logger.Log("JFR recording %s has not finished writing yet; checking again in %s", name, interval)
		}

		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(interval)
	}

	return fmt.Errorf("could not confirm within %s that the JVM had finished writing JFR recording %s; %s", timeout, name, last)
}

// jfrRecordingFinished reports whether JFR.check's listing shows the JVM done
// with the named recording, or an error if checkOutput is not a listing at all
// (a killed jcmd leaves only its "<pid>:" header; a JVM without JFR answers
// with an error message). A listing has one line per recording,
//
//	Recording 2: name=ycJFR-4321-1788156788942701200 duration=60s (running)
//
// with the name quoted on older Oracle JDKs, or a "No available recordings."
// line. The JVM closes a written recording and drops it from the listing, so
// only "(closed)" or absence means finished. "(stopped)" does not: the JVM
// stops the recording before it writes the file.
func jfrRecordingFinished(checkOutput, name string) (bool, error) {
	bare, quoted := "name="+name, "name=\""+name+"\""
	isListing := false

	for _, line := range strings.Split(checkOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "No available recordings") {
			isListing = true
			continue
		}
		if !strings.HasPrefix(line, "Recording") {
			continue
		}
		isListing = true

		// Field-by-field so that a recording whose name merely starts with
		// name - jfrRecordingPath's timestamps are not fixed width - can't be
		// mistaken for this one.
		for _, f := range strings.Fields(line) {
			if f == bare || f == quoted {
				return strings.Contains(line, "(closed)"), nil
			}
		}
	}

	if !isListing {
		return false, errors.New("no recording listing in the response")
	}
	return true, nil
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
