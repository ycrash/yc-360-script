package capture

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"yc-agent/internal/logger"
)

// jfrSettings and excludeSensitiveData. JDK behavior here was tested on
// Temurin 8u512, 11, 17 and 21; testdata/jfr holds the real replies.

// jfrTemplates are the JDK's built-in settings, passed to JFR.start by name.
var jfrTemplates = []string{"default", "profile"}

// IsJFRTemplate reports whether value names a built-in template, not a file.
func IsJFRTemplate(value string) bool {
	_, ok := jfrTemplate(value)
	return ok
}

func jfrTemplate(value string) (string, bool) {
	v := strings.TrimSpace(value)
	for _, t := range jfrTemplates {
		if strings.EqualFold(v, t) {
			return t, true
		}
	}
	return "", false
}

// jfrSensitiveEvents hold environment variables and system properties.
var jfrSensitiveEvents = []string{"jdk.InitialEnvironmentVariable", "jdk.InitialSystemProperty"}

// jfrExclusionSettings turns the sensitive events off. It goes last, since
// the later settings file wins, and never alone, since a settings file turns
// off every event it doesn't name.
const jfrExclusionSettings = `<?xml version="1.0" encoding="UTF-8"?>
<configuration version="2.0" label="yc exclusions">
  <event name="jdk.InitialEnvironmentVariable">
    <setting name="enabled">false</setting>
  </event>
  <event name="jdk.InitialSystemProperty">
    <setting name="enabled">false</setting>
  </event>
</configuration>
`

// jfcMaxSize caps a customer's .jfc file. The JDK's own are about 40 KB.
const jfcMaxSize = 4 << 20

// jfrSettings is a resolved jfrSettings value: zero for the JDK default, a
// template, or a .jfc file's contents.
type jfrSettings struct {
	template string
	jfc      []byte
	source   string // the customer's path, for messages
}

// resolveJFRSettings resolves a jfrSettings value. An unusable .jfc file is
// logged as an error and the JDK default is used instead.
func resolveJFRSettings(value string) jfrSettings {
	v := strings.TrimSpace(value)
	if v == "" {
		return jfrSettings{}
	}
	if t, ok := jfrTemplate(v); ok {
		return jfrSettings{template: t}
	}

	data, err := readJFC(v)
	if err != nil {
		logger.Error().Msgf("jfrSettings: cannot use the JFR settings file %s: %v. Using the JDK's default JFR settings instead.", v, err)
		return jfrSettings{}
	}
	return jfrSettings{jfc: data, source: v}
}

// readJFC reads and checks a .jfc file. yc does this itself because the
// JVM's error doesn't say what's wrong and can name the wrong file.
func readJFC(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		var pathErr *os.PathError
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil, errors.New("the file does not exist")
		case errors.As(err, &pathErr):
			return nil, fmt.Errorf("the file can't be read (%v)", pathErr.Err)
		default:
			return nil, fmt.Errorf("the file can't be read (%v)", err)
		}
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("the file can't be read (%v)", err)
	}
	if info.IsDir() {
		return nil, errors.New("it is a directory, not a .jfc file")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("it is not a regular file")
	}

	data, err := io.ReadAll(io.LimitReader(f, jfcMaxSize+1))
	if err != nil {
		return nil, fmt.Errorf("the file can't be read (%v)", err)
	}
	if len(data) > jfcMaxSize {
		return nil, fmt.Errorf("it is larger than %d MB, which is too large for a JFR settings file", jfcMaxSize>>20)
	}
	if err := validateJFC(data); err != nil {
		return nil, err
	}
	return data, nil
}

// validateJFC checks for well-formed XML with one <configuration> root and at
// least one <event>. The JVM accepts any well-formed XML and records nothing.
func validateJFC(data []byte) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Only the structure matters, so ignore the declared encoding.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	root, depth, events := "", 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("it is not valid XML (%v)", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if root != "" {
					return errors.New("it is not valid XML (more than one root element)")
				}
				root = el.Name.Local
			} else if el.Name.Local == "event" {
				events++
			}
			depth++
		case xml.EndElement:
			depth--
		}
	}

	switch {
	case root == "":
		return errors.New("it contains no XML elements")
	case root != "configuration":
		return fmt.Errorf("its root element is <%s>, but a .jfc file has a <configuration> root element", root)
	case events == 0:
		return errors.New("it has no <event> settings, so the recording would contain no events")
	}
	return nil
}

// jfrSettingsArgs returns the settings= values: base, then the exclusions,
// which must come last and need "default" when there's no base.
func jfrSettingsArgs(base, exclusions string) []string {
	var args []string
	if base != "" {
		args = append(args, base)
	}
	if exclusions != "" {
		if base == "" {
			args = append(args, "default")
		}
		args = append(args, exclusions)
	}
	return args
}

// jfrSettingsFile is a .jfc file yc wrote for the JVM to read.
type jfrSettingsFile struct {
	path   string   // the path JFR.start gives the JVM
	copies []string // every file written, all removed by remove
}

// writeJFRSettingsFile writes data to a world-readable .jfc file in the temp
// directory for the JVM to read. On Linux, a JVM with a different /tmp (in a
// container) also gets a copy at the same path in its mount namespace.
func writeJFRSettingsFile(pid int, prefix string, data []byte) (*jfrSettingsFile, error) {
	f, err := os.CreateTemp("", prefix+"-*.jfc")
	if err != nil {
		return nil, fmt.Errorf("could not create a JFR settings file in %s: %w", os.TempDir(), err)
	}
	sf := &jfrSettingsFile{path: f.Name(), copies: []string{f.Name()}}

	_, err = f.Write(data)
	if err == nil {
		// The JVM may run as another user; the file holds no secrets.
		err = f.Chmod(0o644)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		sf.remove()
		return nil, fmt.Errorf("could not write the JFR settings file %s: %w", sf.path, err)
	}
	if !jfrPathUsable(sf.path) {
		sf.remove()
		return nil, fmt.Errorf("the JFR settings file path %q contains a quote or newline, which jcmd can't express", sf.path)
	}

	if runtime.GOOS == "linux" {
		if nsPath, ok := jfrNamespacePath(pid, sf.path); ok {
			if err := writeNewFile(nsPath, data); err != nil {
				logger.Log("WARNING: could not write the JFR settings file into the target's mount namespace at %s: %v", nsPath, err)
			} else {
				sf.copies = append(sf.copies, nsPath)
			}
		}
	}
	return sf, nil
}

// jfrNamespacePath returns p's path in pid's mount namespace, if not ours.
func jfrNamespacePath(pid int, p string) (string, bool) {
	dir := filepath.Dir(p)
	nsDir := filepath.Join("/proc", strconv.Itoa(pid), "root", dir)

	ours, err := os.Stat(dir)
	if err != nil {
		return "", false
	}
	theirs, err := os.Stat(nsDir)
	if err != nil || os.SameFile(ours, theirs) {
		return "", false
	}
	return filepath.Join(nsDir, filepath.Base(p)), true
}

// writeNewFile writes data to p, which must not exist yet, readable by everyone.
func writeNewFile(p string, data []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(0o644) // the umask may have narrowed it
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(p)
	}
	return err
}

// remove deletes every copy; the JVM is done with them once JFR.start replies.
func (f *jfrSettingsFile) remove() {
	if f == nil {
		return
	}
	for _, p := range f.copies {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			logger.Log("WARNING: could not remove the JFR settings file %s: %v", p, err)
		}
	}
}

// jfrSettingsRefusals start the JVM's reply to an unusable settings file: the
// first for every problem on JDK 8-17, the others on JDK 21.
var jfrSettingsRefusals = []string{
	"Could not parse settings file '",
	"Could not find file '",
	"Could not parse file '",
	"Could not access file '",
	"Could not open file '",
}

// jfrSettingsRefused reports whether reply refuses one of settings. JDK 8-17
// name the first file whichever was bad; Windows paths may come back with
// doubled backslashes.
func jfrSettingsRefused(reply string, settings []string) bool {
	for _, phrase := range jfrSettingsRefusals {
		for _, s := range settings {
			if strings.Contains(reply, phrase+s+"'") || strings.Contains(reply, phrase+strings.ReplaceAll(s, `\`, `\\`)+"'") {
				return true
			}
		}
	}
	return false
}

// jfrSettingsRefusedError is a start refused for its settings, leaving no recording.
type jfrSettingsRefusedError struct {
	reply string
	err   error
}

func (e *jfrSettingsRefusedError) Error() string { return e.err.Error() }
func (e *jfrSettingsRefusedError) Unwrap() error { return e.err }

// jfrListedRecording is a recording in a JFR.check verbose=true listing.
// enabled maps each sensitive event it lists to its enabled value ("" if unset).
type jfrListedRecording struct {
	row     string
	fields  []string
	state   string
	enabled map[string]string
}

func (r jfrListedRecording) named(name string) bool {
	return slices.Contains(r.fields, "name="+name) || slices.Contains(r.fields, "name=\""+name+"\"")
}

// parseJFRCheckVerbose parses JFR.check verbose=true output: unindented
// recording rows, each followed by indented event labels and settings (some
// labels start with "Recording", so indentation matters):
//
//	Recording 14: name=customer maxsize=250.0MB (running)
//	 Initial Environment Variable (jdk.InitialEnvironmentVariable)
//	   [enabled=true,period=beginChunk]
//
// With no recordings the JVM prints nothing: jcmd says "Command executed
// successfully" and jattach prints only its banner. Other output that isn't
// a listing is an error, as it could hide a recording.
func parseJFRCheckVerbose(out string) ([]jfrListedRecording, error) {
	var recs []jfrListedRecording
	isListing := false
	answeredEmpty, other := false, false
	pending := "" // the sensitive event whose settings line comes next

	for raw := range strings.SplitSeq(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "No available recordings") {
			isListing = true
			continue
		}
		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") && strings.HasPrefix(line, "Recording") {
			fields, state, ok := jfrRecordingRow(line)
			if !ok {
				return nil, fmt.Errorf("unrecognised recording row in the response: %q", line)
			}
			isListing = true
			recs = append(recs, jfrListedRecording{row: line, fields: fields, state: state, enabled: map[string]string{}})
			pending = ""
			continue
		}
		if len(recs) == 0 {
			switch {
			case line == "Command executed successfully", line == "JVM response code = 0":
				answeredEmpty = true
			case line == "Connected to remote JVM" || jcmdPidHeader(line):
				// The reply starts here; earlier lines (yc's own logs, via
				// -jCmdCaptureMode) aren't the JVM's.
				other = false
			default:
				other = true
			}
			continue
		}

		cur := &recs[len(recs)-1]
		if pending != "" && strings.HasPrefix(line, "[") {
			cur.enabled[pending] = jfrSettingValue(line, "enabled")
			pending = ""
			continue
		}
		pending = ""
		if ev, ok := jfrEventLabel(line); ok && slices.Contains(jfrSensitiveEvents, ev) {
			cur.enabled[ev] = ""
			pending = ev
		}
	}

	switch {
	case isListing:
		return recs, nil
	case answeredEmpty && !other:
		return nil, nil
	default:
		return nil, errors.New("no recording listing in the response")
	}
}

// jcmdPidHeader reports whether line is jcmd's "<pid>:" header.
func jcmdPidHeader(line string) bool {
	digits, ok := strings.CutSuffix(line, ":")
	if !ok || digits == "" {
		return false
	}
	_, err := strconv.Atoi(digits)
	return err == nil
}

// jfrEventLabel returns the event name from a line like "Label (jdk.Name)".
func jfrEventLabel(line string) (string, bool) {
	if !strings.HasSuffix(line, ")") {
		return "", false
	}
	open := strings.LastIndex(line, " (")
	if open < 0 {
		return "", false
	}
	name := line[open+2 : len(line)-1]
	if name == "" || strings.ContainsAny(name, " ()") {
		return "", false
	}
	return name, true
}

// jfrSettingValue returns key's value from "[enabled=true,...]", or "" if unset.
func jfrSettingValue(line, key string) string {
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
	for kv := range strings.SplitSeq(inner, ",") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(kv), key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// jfrSensitiveRecordings lists recordings, other than skip and stopped or
// closed ones, with a sensitive event on or without enabled=. Recordings
// share one event stream, so their events land in yc's file too.
func jfrSensitiveRecordings(recs []jfrListedRecording, skip string) []string {
	return jfrRecordingsWithSensitiveOn(recs, skip, false)
}

// jfrRecordingsWithSensitiveOn is jfrSensitiveRecordings, also listing
// stopped and closed recordings if ended is set.
func jfrRecordingsWithSensitiveOn(recs []jfrListedRecording, skip string, ended bool) []string {
	var found []string
	for _, r := range recs {
		if (skip != "" && r.named(skip)) || (!ended && (r.state == "stopped" || r.state == "closed")) {
			continue
		}
		var on []string
		for _, ev := range jfrSensitiveEvents {
			if v, listed := r.enabled[ev]; listed && v != "false" {
				on = append(on, ev)
			}
		}
		if len(on) > 0 {
			found = append(found, fmt.Sprintf("%q has %s turned on", r.row, strings.Join(on, " and ")))
		}
	}
	return found
}

// jfrOtherRecordsSensitive starts the error for another recording that has
// the sensitive events on.
const jfrOtherRecordsSensitive = "another JFR recording in the JVM records environment variables or system properties"

// jfrConfirmExcluded checks that recording name lists both sensitive events
// with enabled=false. Unlisted fails: a JFR with other event names (old
// Oracle JDK 8) could still record the data.
func jfrConfirmExcluded(recs []jfrListedRecording, name string) error {
	i := slices.IndexFunc(recs, func(r jfrListedRecording) bool { return r.named(name) })
	if i < 0 {
		return fmt.Errorf("JFR.check does not list recording %s", name)
	}
	for _, ev := range jfrSensitiveEvents {
		v, listed := recs[i].enabled[ev]
		switch {
		case !listed:
			return fmt.Errorf("JFR.check does not list %s in the settings of recording %s", ev, name)
		case v != "false":
			return fmt.Errorf("JFR.check shows %s with enabled=%s in recording %s", ev, v, name)
		}
	}
	return nil
}

// checkNoSensitiveRecordings fails if another recording has a sensitive event
// on, or JFR.check can't tell.
func checkNoSensitiveRecordings(check func() (string, error)) error {
	out, err := check()
	if err != nil {
		return fmt.Errorf("could not check the JVM's other JFR recordings: %w", err)
	}
	recs, err := parseJFRCheckVerbose(out)
	if err != nil {
		return fmt.Errorf("could not read the JVM's other JFR recordings from JFR.check: %v", err)
	}
	if found := jfrSensitiveRecordings(recs, ""); len(found) > 0 {
		return fmt.Errorf("%s: %s. JFR recordings share their data, so yc's recording would contain the values too. Stop that recording, or run yc without -excludeSensitiveData", jfrOtherRecordsSensitive, strings.Join(found, "; "))
	}
	return nil
}

// confirmExclusions checks, after the start, that recording name has both
// sensitive events off and no other recording has them on.
func confirmExclusions(name string, check func() (string, error)) error {
	out, err := check()
	if err != nil {
		return fmt.Errorf("could not check the recording's settings: %w", err)
	}
	recs, err := parseJFRCheckVerbose(out)
	if err != nil {
		return fmt.Errorf("could not read the recording's settings from JFR.check: %v", err)
	}
	if err := jfrConfirmExcluded(recs, name); err != nil {
		return err
	}
	if found := jfrSensitiveRecordings(recs, name); len(found) > 0 {
		return fmt.Errorf("%s: %s", jfrOtherRecordsSensitive, strings.Join(found, "; "))
	}
	return nil
}

// checkRecordingExcluded reads recording name's finished file p and fails if
// it holds a sensitive event. A recording that runs during yc's window, even
// briefly, puts them into yc's file, and JFR.check no longer lists one that
// has ended. check is used only to name the recording.
func checkRecordingExcluded(name, p string, check func() (string, error)) error {
	counts, err := jfrEventCounts(p, jfrSensitiveEvents)
	if err != nil {
		return fmt.Errorf("yc could not read the recording to check it for %s events: %v", strings.Join(jfrSensitiveEvents, " and "), err)
	}
	var found []string
	for _, ev := range jfrSensitiveEvents {
		if counts[ev] > 0 {
			found = append(found, fmt.Sprintf("%d %s", counts[ev], ev))
		}
	}
	if len(found) == 0 {
		logger.Log("JFR recording %s checked: it holds no %s events", name, strings.Join(jfrSensitiveEvents, " or "))
		return nil
	}

	msg := fmt.Sprintf("the recording holds %s events, so another JFR recording in the JVM recorded environment variables or system properties while yc's recording was running", strings.Join(found, " and "))
	if out, err := check(); err == nil {
		if recs, err := parseJFRCheckVerbose(out); err == nil {
			if others := jfrRecordingsWithSensitiveOn(recs, name, true); len(others) > 0 {
				msg += ": " + strings.Join(others, "; ")
			}
		}
	}
	return errors.New(msg)
}
