package capture

import (
	"bytes"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/logger"
)

// jfrFileName is the fixed filename yc-agent uses for the JFR recording.
const jfrFileName = "my.jfr"

const (
	jfrDefaultDuration = 60 * time.Second
	jfrMinDuration     = 10 * time.Second
	jfrMaxDuration     = 5 * time.Minute
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
// The recording is started immediately and kept running for Duration, then
// explicitly stopped (JFR.stop) before being read and transmitted. FullCapture
// reads its result last, so the recording window overlaps the rest of the
// capture instead of adding to it.
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

// CaptureToFile starts a JFR recording, lets it run for the configured
// duration, explicitly stops it, then opens the resulting recording file.
func (t *JFR) CaptureToFile() (*os.File, error) {
	if !IsProcessExists(t.Pid) {
		return nil, fmt.Errorf("process %d does not exist", t.Pid)
	}

	// Unique per invocation (pid + timestamp), not a fixed constant: back to
	// back runs against the same long-lived JVM (e.g. repeated manual
	// testing) must never collide with a still-active recording left behind
	// by a previous run.
	name := fmt.Sprintf("ycJFR-%d-%d", t.Pid, time.Now().UnixNano())

	requestedPath, err := t.startRecording(name)
	if err != nil {
		return nil, err
	}

	duration := t.effectiveDuration()
	logger.Log("JFR recording %s started, running for %s", name, duration)
	time.Sleep(duration)

	// Stop synchronously - jcmd/jattach don't return until the JVM has
	// processed the command, so this guarantees the recording is flushed and
	// its file handle closed before we try to read it below.
	if err := t.stopRecording(name); err != nil {
		return nil, fmt.Errorf("failed to stop JFR recording: %w", err)
	}

	return t.openRecording(requestedPath)
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

// startRecording starts an open-ended JFR recording named name on the target
// JVM (no duration= - it keeps running until stopRecording stops it) and
// returns the absolute path the recording is being written to.
func (t *JFR) startRecording(name string) (string, error) {
	requestedPath, err := filepath.Abs(jfrFileName)
	if err != nil {
		requestedPath = jfrFileName
	}

	cmd := fmt.Sprintf("JFR.start name=%s filename=%s", name, requestedPath)
	if _, err := t.runJcmd(cmd); err != nil {
		return "", fmt.Errorf("failed to start JFR recording: %w", err)
	}

	return requestedPath, nil
}

// stopRecording stops the JFR recording named name, blocking until the JVM
// confirms the stop (see CaptureToFile for why this matters).
func (t *JFR) stopRecording(name string) error {
	_, err := t.runJcmd(fmt.Sprintf("JFR.stop name=%s", name))
	return err
}

// openRecording opens the JFR recording file written by startRecording.
func (t *JFR) openRecording(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open JFR recording %s: %w", path, err)
	}

	return file, nil
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
