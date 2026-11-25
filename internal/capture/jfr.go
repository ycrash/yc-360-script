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
	"slices"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/config"
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

// jfrStartedReply confirms JFR.start worked. A failure replies with only the
// exception message (e.g. "Java heap space") and jcmd still exits 0.
const jfrStartedReply = "Started recording"

// JFR captures a JVM Flight Recorder recording for a running Java process.
// The recording is started with duration=, so the JVM stops it and writes it
// out on its own after Duration - the agent never sends JFR.stop, and waits
// for JFR.check to report the recording finished before it touches the file.
// The JVM writes to the temp directory (jfrRecordingPath); yc then stages the
// result into the capture directory as jfrFileName. FullCapture reads its
// result last, so the recording window overlaps the rest of the capture
// instead of adding to it. jfr_settings.go handles Settings and
// ExcludeSensitiveData.
type JFR struct {
	Capture
	Pid      int
	JavaHome string
	// Duration is how long the recording runs. Zero means jfrDefaultDuration.
	Duration time.Duration
	// Settings is "" (JDK default), a template (default, profile) or a .jfc path.
	Settings string
	// ExcludeSensitiveData keeps the jfrSensitiveEvents out of the recording.
	ExcludeSensitiveData bool

	// jcmd replaces runJcmd in tests.
	jcmd func(command string) (string, error)
	// jcmdFrom is where runJcmd starts: once a fallback has worked, the
	// methods before it are skipped. On JDK 11, jcmd as root against
	// another user's JVM takes 10.5s to fail every time.
	jcmdFrom jcmdVia
	// runCmd replaces executils.CommandCombinedOutputToWriter in tests.
	runCmd func(w io.Writer, cmd executils.Command, hookers ...executils.Hooker) error
}

// jcmdVia is a way runJcmd sends a command, in the order it tries them.
type jcmdVia int

const (
	viaJcmd       jcmdVia = iota // $JAVA_HOME/bin/jcmd
	viaJattach                   // yc -jCmdCaptureMode
	viaTmpJattach                // the same from a temp copy of yc
)

var jcmdViaNames = [...]string{viaJcmd: "jcmd", viaJattach: "jattach", viaTmpJattach: "tmp jattach"}

// useJcmdVia makes via, which just worked, where runJcmd starts from now on.
func (t *JFR) useJcmdVia(via jcmdVia) {
	if via > t.jcmdFrom {
		logger.Log("%s worked after %s failed; sending the rest of this JFR capture's commands to PID %d through %s",
			jcmdViaNames[via], jcmdViaNames[t.jcmdFrom], t.Pid, jcmdViaNames[via])
		t.jcmdFrom = via
	}
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

	// Skip in MinimalTouch mode, same as the jstat/jattach GC capture and
	// hdsub: a JFR.start recording runs on the target JVM for t.Duration
	// (default 60s), so even at default.jfc's lower sampling rate it's more
	// touch than a point-in-time capture.
	if config.GlobalConfig.MinimalTouch {
		return nil, errors.New("MinimalTouch mode: skipping JFR capture (uses jcmd JFR.start, which is CPU-intensive)")
	}

	duration := t.effectiveDuration()

	name, jvmPath, err := t.start(duration)
	if err != nil {
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

	// Unless the JVM shows the exclusions in force, the recording runs out
	// (yc never sends JFR.stop) and is deleted instead of uploaded.
	var discard error
	if t.ExcludeSensitiveData {
		if err := confirmExclusions(name, t.checkRecordingsVerbose); err != nil {
			discard = fmt.Errorf("discarding JFR recording %s: excludeSensitiveData is on, but %v", name, err)
			logger.Log("WARNING: %v. yc will delete the recording once the JVM has finished writing it, and will not upload it", discard)
		}
	}

	logger.Log("JFR recording %s started, running for %s; then checking every %s, for up to %s, that the JVM has written it",
		name, duration, jfrCheckInterval, jfrCheckTimeout)
	time.Sleep(duration)

	if err := t.waitForRecording(name); err != nil {
		if !errors.Is(err, errJVMGone) {
			logger.Log("WARNING: leaving %s alone; the JVM may still be writing the recording there", jvmPath)
			if discard != nil {
				return nil, fmt.Errorf("%v; %w", discard, err)
			}
			return nil, err
		}
		// Nobody can be writing the file once the JVM is gone: an empty one
		// is the stub JFR.start created, anything else is the recording.
		finished = true
		if discard != nil {
			if resolved, err := resolveRecordingPath(t.Pid, jvmPath); err == nil {
				sourcePath = resolved
			}
			return nil, discard
		}
		info, statErr := os.Stat(jvmPath)
		if statErr != nil || info.Size() == 0 {
			return nil, fmt.Errorf("the target JVM exited before it wrote JFR recording %s", name)
		}
		logger.Log("the JVM exited before JFR.check could confirm recording %s; staging the %d bytes it wrote", name, info.Size())
	}
	finished = true

	resolved, err := resolveRecordingPath(t.Pid, jvmPath)
	if err == nil {
		sourcePath = resolved
	}
	if discard != nil {
		return nil, discard
	}
	if err != nil {
		return nil, err
	}

	// Another recording that ran during the window put its events in too.
	if t.ExcludeSensitiveData {
		if err := checkRecordingExcluded(name, sourcePath, t.checkRecordingsVerbose); err != nil {
			err = fmt.Errorf("discarding JFR recording %s: excludeSensitiveData is on, but %v", name, err)
			logger.Log("WARNING: %v. yc deletes the recording and will not upload it", err)
			return nil, err
		}
	}

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

// start starts the recording with the jfrSettings settings, plus the
// exclusions if ExcludeSensitiveData is set. It skips JFR rather than record
// without the exclusions, and retries refused settings once with the JDK
// default (a refused start creates no recording).
func (t *JFR) start(duration time.Duration) (name, jvmPath string, err error) {
	if t.ExcludeSensitiveData {
		if err := checkNoSensitiveRecordings(t.checkRecordingsVerbose); err != nil {
			return "", "", fmt.Errorf("skipping JFR capture: excludeSensitiveData is on, but %v", err)
		}
	}

	settings := resolveJFRSettings(t.Settings)
	base := settings.template
	if settings.jfc != nil {
		f, err := writeJFRSettingsFile(t.Pid, "yc-jfr-settings", settings.jfc)
		if err != nil {
			logger.Error().Msgf("jfrSettings: cannot use the JFR settings file %s: %v. Using the JDK's default JFR settings instead.", settings.source, err)
		} else {
			defer f.remove()
			base = f.path
		}
	}

	exclusions := ""
	if t.ExcludeSensitiveData {
		f, err := writeJFRSettingsFile(t.Pid, "yc-jfr-exclusions", []byte(jfrExclusionSettings))
		if err != nil {
			return "", "", fmt.Errorf("skipping JFR capture: excludeSensitiveData is on, but %v", err)
		}
		defer f.remove()
		exclusions = f.path
	}

	name, jvmPath, err = t.startRecording(duration, jfrSettingsArgs(base, exclusions))
	var refused *jfrSettingsRefusedError
	if errors.As(err, &refused) && base != "" {
		logger.Error().Msgf("jfrSettings: the JVM could not use the JFR settings %s (it answered %q). Starting the recording again with the JDK's default JFR settings.", t.Settings, refused.reply)
		name, jvmPath, err = t.startRecording(duration, jfrSettingsArgs("", exclusions))
	}
	if errors.As(err, &refused) && exclusions != "" {
		return "", "", fmt.Errorf("skipping JFR capture: excludeSensitiveData is on, but the JVM refused the settings file that turns off %s: %w",
			strings.Join(jfrSensitiveEvents, " and "), err)
	}
	return name, jvmPath, err
}

// startRecording starts a recording with the given settings= values and
// returns its name and the path the JVM writes it to.
//
// duration= puts the timer inside the JVM: it stops the recording and writes
// the file with nobody attached, so the agent needs only this one diagnostic
// command, and a recording can't outlive the agent if yc is killed partway
// through.
func (t *JFR) startRecording(duration time.Duration, settings []string) (name, jvmPath string, err error) {
	// Unique per invocation (pid + timestamp), not a fixed constant: back to
	// back runs against the same long-lived JVM (e.g. repeated manual
	// testing) must never collide with a still-active recording left behind
	// by a previous run.
	nanos := time.Now().UnixNano()
	name = fmt.Sprintf("ycJFR-%d-%d", t.Pid, nanos)
	jvmPath = jfrRecordingPath(t.Pid, nanos)
	if !jfrPathUsable(jvmPath) {
		return "", "", fmt.Errorf("JFR recording path %q contains a quote or newline, which jcmd can't express", jvmPath)
	}

	out, err := t.jcmdRun(jfrStartCommand(name, jvmPath, duration, settings))
	if err != nil {
		return "", "", fmt.Errorf("failed to start JFR recording: %w", err)
	}
	if strings.Contains(out, jfrStartedReply) {
		return name, jvmPath, nil
	}

	// Not in runJcmd: its jattach fallbacks would resend JFR.start.
	reply := jcmdReply(t.Pid, out)
	err = settleUnconfirmedStart(t.Pid, name, jvmPath, reply, t.checkRecordings)
	var notListed *jfrNotListedError
	if errors.As(err, &notListed) && jfrSettingsRefused(reply, settings) {
		return "", "", &jfrSettingsRefusedError{reply: reply, err: err}
	}
	if err != nil {
		return "", "", err
	}
	return name, jvmPath, nil
}

// jfrStartCommand builds the JFR.start command. Each settings value is its own
// settings= argument (a comma-separated list fails on every JDK); templates go
// by name, files as quoted paths. No settings means the JDK's default.jfc.
func jfrStartCommand(name, jvmPath string, duration time.Duration, settings []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "JFR.start %s %s duration=%s", jfrArg("name", name), jfrArg("filename", jvmPath), jfrTimespan(duration))
	for _, s := range settings {
		b.WriteByte(' ')
		if IsJFRTemplate(s) {
			b.WriteString("settings=")
			b.WriteString(s)
		} else {
			b.WriteString(jfrArg("settings", s))
		}
	}
	return b.String()
}

// jcmdReply strips jcmd's "<pid>:" header from out.
func jcmdReply(pid int, out string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), strconv.Itoa(pid)+":"))
}

// settleUnconfirmedStart resolves an unconfirmed JFR.start with one JFR.check,
// since the JVM can throw after starting the recording. Running returns nil;
// anything else an error quoting reply. The empty file is removed only when
// nothing can write to it: deleting it under a running recording destroys it.
func settleUnconfirmedStart(pid int, name, jvmPath, reply string, check func() (string, error)) error {
	fail := func(why string) error {
		return fmt.Errorf("failed to start JFR recording: the JVM answered JFR.start with %q; %s", reply, why)
	}

	out, err := check()
	if errors.Is(err, errJVMGone) {
		removeUnstartedRecording(pid, jvmPath)
		return fail("the target JVM has since exited")
	}
	if err != nil {
		return fail(fmt.Sprintf("could not check whether recording %s started: %v", name, err))
	}

	state, err := jfrRecordingState(out, name)
	switch {
	case err != nil:
		return fail(fmt.Sprintf("could not check whether recording %s started: JFR.check answered %q", name, jcmdReply(pid, out)))
	case state == "running":
		logger.Log("WARNING: JFR.start returned %q, but JFR.check confirmed recording %s is running; continuing capture", reply, name)
		return nil
	case state == "":
		removeUnstartedRecording(pid, jvmPath)
		return &jfrNotListedError{fail(fmt.Sprintf("JFR.check does not list recording %s", name))}
	case state == "new":
		removeUnstartedRecording(pid, jvmPath)
		return fail(fmt.Sprintf("JFR.check lists recording %s as new, never started", name))
	default:
		return fail(fmt.Sprintf("JFR.check lists recording %s as %s", name, state))
	}
}

// jfrNotListedError is a failed start that JFR.check confirmed left no
// recording, so starting again is safe.
type jfrNotListedError struct{ error }

func (e *jfrNotListedError) Unwrap() error { return e.error }

// removeUnstartedRecording deletes jvmPath, here or in the target's mount
// namespace, if it is empty. Call it only once no recording can write there.
func removeUnstartedRecording(pid int, jvmPath string) {
	paths := []string{jvmPath}
	if runtime.GOOS == "linux" {
		paths = append(paths, filepath.Join("/proc", strconv.Itoa(pid), "root", jvmPath))
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Log("WARNING: could not remove the empty JFR recording file %s: %v", p, err)
		}
	}
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
	return waitForRecording(name, jfrCheckTimeout, jfrCheckInterval, t.checkRecordings)
}

// checkRecordings runs JFR.check, returning errJVMGone if the target is gone.
func (t *JFR) checkRecordings() (string, error) {
	return t.check("JFR.check")
}

// checkRecordingsVerbose is checkRecordings with each recording's event settings.
func (t *JFR) checkRecordingsVerbose() (string, error) {
	return t.check("JFR.check verbose=true")
}

func (t *JFR) check(command string) (string, error) {
	out, err := t.jcmdRun(command)
	if err != nil && !IsProcessExists(t.Pid) {
		return "", errJVMGone
	}
	return out, err
}

func (t *JFR) jcmdRun(command string) (string, error) {
	if t.jcmd != nil {
		return t.jcmd(command)
	}
	return t.runJcmd(command)
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
// with the named recording, or an error if checkOutput is not a listing at
// all. The JVM closes a written recording and drops it from the listing, so
// only "(closed)" or absence means finished. "(stopped)" does not: the JVM
// stops the recording before it writes the file.
func jfrRecordingFinished(checkOutput, name string) (bool, error) {
	state, err := jfrRecordingState(checkOutput, name)
	if err != nil {
		return false, err
	}
	return state == "" || state == "closed", nil
}

// jfrStates are the states JFR.check prints, e.g. "(running)".
var jfrStates = []string{"new", "delayed", "running", "stopped", "closed"}

// jfrRecordingState returns the named recording's state: one of jfrStates,
// "unknown", or "" if not listed. It errors on output that isn't a listing
// (a lone "<pid>:" header, a JVM error) or has a malformed row, which could
// hide the recording. A listing has one row per recording,
//
//	Recording 2: name=ycJFR-4321-1788156788942701200 duration=60s (running)
//
// with the name quoted on older Oracle JDKs, or a "No available recordings."
// line.
func jfrRecordingState(checkOutput, name string) (string, error) {
	bare, quoted := "name="+name, "name=\""+name+"\""
	isListing := false
	malformed := ""

	for _, line := range strings.Split(checkOutput, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "No available recordings") {
			isListing = true
			continue
		}
		if !strings.HasPrefix(line, "Recording") {
			continue
		}
		fields, state, ok := jfrRecordingRow(line)
		if !ok {
			malformed = line
			continue
		}
		isListing = true

		// Field-by-field so that a recording whose name merely starts with
		// name - jfrRecordingPath's timestamps are not fixed width - can't be
		// mistaken for this one.
		if slices.Contains(fields, bare) || slices.Contains(fields, quoted) {
			if slices.Contains(jfrStates, state) {
				return state, nil
			}
			return "unknown", nil
		}
	}

	switch {
	case malformed != "":
		return "", fmt.Errorf("unrecognised recording row in the response: %q", malformed)
	case !isListing:
		return "", errors.New("no recording listing in the response")
	default:
		return "", nil
	}
}

// jfrRecordingRow returns a row's fields and lower-cased state, taken from the
// last field only (filename= may contain "(new)"). ok is false unless the row
// has a name= field and ends in a state.
func jfrRecordingRow(line string) (fields []string, state string, ok bool) {
	fields = strings.Fields(line)
	last := fields[len(fields)-1]
	if len(last) < 3 || !strings.HasPrefix(last, "(") || !strings.HasSuffix(last, ")") {
		return nil, "", false
	}
	hasName := slices.ContainsFunc(fields[:len(fields)-1], func(f string) bool {
		return strings.HasPrefix(f, "name=")
	})
	if !hasName {
		return nil, "", false
	}
	return fields, strings.ToLower(last[1 : len(last)-1]), true
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
// HDSub.executeJcmd). Once a fallback works, later calls start from it
// (jcmdFrom). Every attempt goes through the timeout-bounded
// executils.CommandCombinedOutputToWriter (config.GlobalConfig.CmdTimeout),
// so a hung jcmd/jattach process can't block the capture indefinitely. It
// returns the command's response text on success.
//
// A 0 exit code only means jcmd/jattach successfully delivered the command;
// it says nothing about whether the JVM actually honored it (e.g. JFR.start
// prints an error and exits 0 if a recording by that name/id can't be
// started). jcmdSucceeded checks the response text for that.
func (t *JFR) runJcmd(command string) (string, error) {
	run := t.runCmd
	if run == nil {
		run = executils.CommandCombinedOutputToWriter
	}
	var out bytes.Buffer

	if t.jcmdFrom <= viaJcmd {
		err := run(&out,
			executils.Command{path.Join(t.JavaHome, "bin/jcmd"), strconv.Itoa(t.Pid), command},
			executils.SudoHooker{PID: t.Pid})
		if err == nil && jcmdSucceeded(out.String()) {
			logger.Log("jcmd %s: %s", command, jcmdLogText(out.String()))
			return out.String(), nil
		}
		logger.Log("jcmd failed: %s. Falling back to jattach for: %s", jcmdFailure(err, out.String()), command)
		out.Reset()
	}

	firstAttemptFailure := "jattach failed earlier in this JFR capture"
	if t.jcmdFrom <= viaJattach {
		err := run(&out,
			executils.Command{executils.Executable(), "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
			executils.EnvHooker{"pid": strconv.Itoa(t.Pid)}, executils.SudoHooker{PID: t.Pid})
		if err == nil && jcmdSucceeded(out.String()) {
			t.useJcmdVia(viaJattach)
			logger.Log("jattach %s: %s", command, jcmdLogText(out.String()))
			return out.String(), nil
		}
		logger.Log("jattach failed: %s. Falling back to tmp jattach for: %s", jcmdFailure(err, out.String()), command)
		firstAttemptFailure = jcmdFailure(err, out.String())
		out.Reset()
	}

	tempPath, tmpErr := executils.Copy2TempPath()
	if tmpErr != nil {
		return "", fmt.Errorf("failed to run %q: %s (tmp jattach fallback failed: %v)", command, firstAttemptFailure, tmpErr)
	}

	err := run(&out,
		executils.Command{tempPath, "-p", strconv.Itoa(t.Pid), "-jCmdCaptureMode", command},
		executils.EnvHooker{"pid": strconv.Itoa(t.Pid)}, executils.SudoHooker{PID: t.Pid})
	if err != nil {
		return "", fmt.Errorf("failed to run %q: %w", command, err) // err already holds the output
	}
	if !jcmdSucceeded(out.String()) {
		return "", fmt.Errorf("failed to run %q: %s", command, jcmdFailure(nil, out.String()))
	}
	t.useJcmdVia(viaTmpJattach)
	logger.Log("tmp jattach %s: %s", command, jcmdLogText(out.String()))
	return out.String(), nil
}

// jcmdLogLines caps the reply lines runJcmd logs; JFR.check verbose=true runs
// to hundreds.
const jcmdLogLines = 40

func jcmdLogText(out string) string {
	if strings.Count(out, "\n") <= jcmdLogLines {
		return out
	}
	lines := strings.SplitAfterN(out, "\n", jcmdLogLines+1)
	return strings.Join(lines[:jcmdLogLines], "") + fmt.Sprintf("... (%d more lines not logged)", strings.Count(lines[jcmdLogLines], "\n"))
}

// jcmdFailure describes a failed jcmd/jattach attempt for a log or error
// message. A command error already includes the output, so the output is
// only added when the command itself succeeded but the JVM rejected it.
// Java stack trace frames are left out.
func jcmdFailure(err error, output string) string {
	if err != nil {
		return err.Error()
	}
	return "JVM rejected the command: " + strings.TrimSpace(executils.TrimJavaStackTrace(output))
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
