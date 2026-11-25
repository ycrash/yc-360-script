package capture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"yc-agent/internal/logger"
)

// jfrFixture reads a real JFR.check verbose=true reply from testdata/jfr.
// two-recordings: "customer" (default, events on) and "leak_repeat" (default
// plus exclusions). one-recording: "verbose_probe", like leak_repeat.
func jfrFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "internal", "capture", "testdata", "jfr", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

var (
	jfrTwoRecordingFixtures = []string{
		"check-verbose-two-recordings-jdk8.txt",
		"check-verbose-two-recordings-jdk11.txt",
		"check-verbose-two-recordings-jdk17.txt",
		"check-verbose-two-recordings-jdk21.txt",
	}
	jfrOneRecordingFixtures = []string{
		"check-verbose-one-recording-jdk8.txt",
		"check-verbose-one-recording-jdk21.txt",
	}
)

// captureLogs redirects the logger to a buffer for the rest of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	l := zerolog.New(&logs)
	saved := logger.GetLogger()
	logger.SetLogger(&l)
	t.Cleanup(func() { logger.SetLogger(saved) })
	return &logs
}

// isolateTempDir points os.TempDir at a fresh, empty directory for the test.
func isolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

const validJFC = `<?xml version="1.0" encoding="UTF-8"?>
<configuration version="2.0" label="Custom" description="A customer's settings" provider="Example">
  <event name="jdk.CPULoad">
    <setting name="enabled">true</setting>
    <setting name="period">1000 ms</setting>
  </event>
  <event name="jdk.InitialEnvironmentVariable">
    <setting name="enabled">true</setting>
    <setting name="period">beginChunk</setting>
  </event>
</configuration>
`

func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIsJFRTemplate(t *testing.T) {
	for _, v := range []string{"default", "profile", "Default", " PROFILE "} {
		if !IsJFRTemplate(v) {
			t.Errorf("IsJFRTemplate(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "default.jfc", "/opt/jfr/default", "./profile", "custom"} {
		if IsJFRTemplate(v) {
			t.Errorf("IsJFRTemplate(%q) = true, want false", v)
		}
	}
}

func TestJFRValidateJFC(t *testing.T) {
	valid := map[string]string{
		"customer file":   validJFC,
		"exclusions file": jfrExclusionSettings,
		"JDK 17 control section": `<?xml version="1.0" encoding="UTF-8"?>
<configuration version="2.0" label="Custom">
  <event name="jdk.CPULoad"><setting name="enabled" control="cpu">true</setting></event>
  <control><selection name="gc" default="normal" label="GC"><option label="Off" name="off">off</option></selection></control>
</configuration>`,
		"declared Latin-1": `<?xml version="1.0" encoding="ISO-8859-1"?><configuration version="2.0"><event name="jdk.CPULoad"/></configuration>`,
	}
	for name, data := range valid {
		if err := validateJFC([]byte(data)); err != nil {
			t.Errorf("%s: validateJFC() = %v, want nil", name, err)
		}
	}

	invalid := []struct{ name, data, want string }{
		// Refused by the JVM.
		{"broken XML", "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<configuration version=\"2.0\" label=\"broken\">\n  <event name=\"jdk.CPULoad\">\n    <setting name=\"enabled\">true\n", "is not valid XML"},
		// Accepted by the JVM, which then records nothing.
		{"not a .jfc", "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<foo/>\n", "root element is <foo>"},
		{"no events", `<configuration version="2.0"/>`, "no <event> settings"},
		{"empty", "", "contains no XML elements"},
		{"plain text", "settings=profile", "contains no XML elements"},
		{"two roots", `<configuration><event name="jdk.CPULoad"/></configuration><configuration/>`, "more than one root element"},
	}
	for _, tt := range invalid {
		err := validateJFC([]byte(tt.data))
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: validateJFC() = %v, want an error containing %q", tt.name, err, tt.want)
		}
	}
}

func TestJFRReadJFC(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		p := writeTestFile(t, "custom.jfc", validJFC)
		data, err := readJFC(p)
		if err != nil || string(data) != validJFC {
			t.Fatalf("readJFC() = %q, %v; want the file's contents", data, err)
		}
	})

	tests := []struct {
		name  string
		setup func(t *testing.T) string
		want  string
	}{
		{"missing", func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope.jfc") }, "the file does not exist"},
		{"directory", func(t *testing.T) string { return t.TempDir() }, "it is a directory"},
		{"not a .jfc", func(t *testing.T) string { return writeTestFile(t, "x.jfc", "<foo/>") }, "root element is <foo>"},
		{"too large", func(t *testing.T) string {
			p := writeTestFile(t, "big.jfc", validJFC)
			if err := os.Truncate(p, jfcMaxSize+1); err != nil {
				t.Fatal(err)
			}
			return p
		}, "larger than 4 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readJFC(tt.setup(t))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("readJFC() = %v, want an error containing %q", err, tt.want)
			}
		})
	}

	t.Run("unreadable", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs a non-root user on a POSIX system")
		}
		p := writeTestFile(t, "locked.jfc", validJFC)
		if err := os.Chmod(p, 0); err != nil {
			t.Fatal(err)
		}
		_, err := readJFC(p)
		if err == nil || !strings.Contains(err.Error(), "can't be read (permission denied)") {
			t.Errorf("readJFC() = %v, want a permission error", err)
		}
	})
}

func TestResolveJFRSettings(t *testing.T) {
	custom := writeTestFile(t, "custom.jfc", validJFC)
	missing := filepath.Join(t.TempDir(), "nope.jfc")

	tests := []struct {
		value        string
		wantTemplate string
		wantJFC      bool
		wantLog      string
	}{
		{value: ""},
		{value: "default", wantTemplate: "default"},
		{value: " Profile ", wantTemplate: "profile"},
		{value: custom, wantJFC: true},
		{value: missing, wantLog: "jfrSettings: cannot use the JFR settings file " + missing + ": the file does not exist. Using the JDK's default JFR settings instead."},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			logs := captureLogs(t)
			got := resolveJFRSettings(tt.value)
			if got.template != tt.wantTemplate || (got.jfc != nil) != tt.wantJFC {
				t.Errorf("resolveJFRSettings(%q) = template %q, jfc %t; want %q, %t", tt.value, got.template, got.jfc != nil, tt.wantTemplate, tt.wantJFC)
			}
			if tt.wantJFC && (string(got.jfc) != validJFC || got.source != custom) {
				t.Errorf("resolveJFRSettings(%q) read %q from %q", tt.value, got.jfc, got.source)
			}
			if tt.wantLog == "" && logs.Len() > 0 {
				t.Errorf("unexpected log: %s", logs)
			}
			if tt.wantLog != "" && (!strings.Contains(logs.String(), tt.wantLog) || !strings.Contains(logs.String(), `"level":"error"`)) {
				t.Errorf("want an error log containing %q; logs: %s", tt.wantLog, logs)
			}
		})
	}
}

func TestJFRSettingsArgs(t *testing.T) {
	tests := []struct {
		base, exclusions string
		want             []string
	}{
		{"", "", nil},
		{"profile", "", []string{"profile"}},
		{"/tmp/c.jfc", "", []string{"/tmp/c.jfc"}},
		// The exclusions alone would turn every other event off.
		{"", "/tmp/x.jfc", []string{"default", "/tmp/x.jfc"}},
		{"default", "/tmp/x.jfc", []string{"default", "/tmp/x.jfc"}},
		{"/tmp/c.jfc", "/tmp/x.jfc", []string{"/tmp/c.jfc", "/tmp/x.jfc"}},
	}
	for _, tt := range tests {
		if got := jfrSettingsArgs(tt.base, tt.exclusions); !slices.Equal(got, tt.want) {
			t.Errorf("jfrSettingsArgs(%q, %q) = %q, want %q", tt.base, tt.exclusions, got, tt.want)
		}
	}
}

func TestJFRStartCommand(t *testing.T) {
	const name, path = "ycJFR-4321-1788156788942701200", "/tmp/my.jfr.4321.1788156788942701200"
	tests := []struct {
		settings []string
		want     string
	}{
		// Neither option: the command yc has always sent.
		{nil, `JFR.start name="ycJFR-4321-1788156788942701200" filename="/tmp/my.jfr.4321.1788156788942701200" duration=60s`},
		{[]string{"profile"}, `JFR.start name="ycJFR-4321-1788156788942701200" filename="/tmp/my.jfr.4321.1788156788942701200" duration=60s settings=profile`},
		// The tested form: bare template, quoted path.
		{[]string{"default", "/tmp/yc-jfr-exclusions-1.jfc"}, `JFR.start name="ycJFR-4321-1788156788942701200" filename="/tmp/my.jfr.4321.1788156788942701200" duration=60s settings=default settings="/tmp/yc-jfr-exclusions-1.jfc"`},
		{[]string{"/tmp/yc-jfr-settings-1.jfc", "/tmp/yc-jfr-exclusions-2.jfc"}, `JFR.start name="ycJFR-4321-1788156788942701200" filename="/tmp/my.jfr.4321.1788156788942701200" duration=60s settings="/tmp/yc-jfr-settings-1.jfc" settings="/tmp/yc-jfr-exclusions-2.jfc"`},
	}
	for _, tt := range tests {
		if got := jfrStartCommand(name, path, 60*time.Second, tt.settings); got != tt.want {
			t.Errorf("jfrStartCommand(%q)\n got %s\nwant %s", tt.settings, got, tt.want)
		}
	}
}

// Real JFR.start replies, "<pid>:" header stripped.
func TestJFRSettingsRefused(t *testing.T) {
	tests := []struct {
		name     string
		reply    string
		settings []string
		want     bool
	}{
		{"8/11/17 missing", "Could not parse settings file '/tmp/jfc/nope.jfc'", []string{"/tmp/jfc/nope.jfc"}, true},
		{"8/11/17 broken", "Could not parse settings file '/tmp/jfc/broken.jfc'", []string{"/tmp/jfc/broken.jfc"}, true},
		{"8/11/17 directory", "Could not parse settings file '/tmp/jfc'", []string{"/tmp/jfc"}, true},
		// JDK 8-17 name the first file, even when the second was bad.
		{"8/11/17 second file missing", "Could not parse settings file 'default'", []string{"default", "/tmp/jfc/nope.jfc"}, true},
		{"21 missing", "Could not find file '/tmp/jfc/nope.jfc'", []string{"/tmp/jfc/nope.jfc"}, true},
		{"21 broken", "Could not parse file '/tmp/jfc/broken.jfc'", []string{"/tmp/jfc/broken.jfc"}, true},
		{"21 unreadable", "Could not access file '/tmp/jfc/unreadable.jfc'", []string{"/tmp/jfc/unreadable.jfc"}, true},
		{"21 directory", "Could not open file '/tmp/jfc'", []string{"/tmp/jfc"}, true},
		{"21 second file missing", "Could not find file '/tmp/jfc/nope.jfc'", []string{"default", "/tmp/jfc/nope.jfc"}, true},
		// Passes validateJFC, refused for its version.
		{"8 version 3.0", "Could not parse settings file '/tmp/jfc/v3.jfc'", []string{"/tmp/jfc/v3.jfc"}, true},
		{"21 version 3.0", "Could not parse file '/tmp/jfc/v3.jfc'. This version of Flight Recorder can only read JFC file format version 2.x", []string{"/tmp/jfc/v3.jfc"}, true},
		{"Windows path as sent", `Could not parse settings file 'C:\\Temp\\yc-jfr-settings-1.jfc'`, []string{`C:\Temp\yc-jfr-settings-1.jfc`}, true},
		{"Windows path normalized", `Could not find file 'C:\Temp\yc-jfr-settings-1.jfc'`, []string{`C:\Temp\yc-jfr-settings-1.jfc`}, true},
		{"names a file it wasn't given", "Could not parse settings file '/tmp/other.jfc'", []string{"/tmp/jfc/x.jfc"}, false},
		{"no settings given", "Could not parse settings file 'default'", nil, false},
		{"started", "Started recording 5. The result will be written to:\n\n/out/jdk8/repeat_default_exclude.jfr", []string{"default", "/tmp/jfc/exclude.jfc"}, false},
		{"heap full", "Java heap space", []string{"default"}, false},
		{"8/11 inline event setting", "java.lang.IllegalArgumentException: Unknown argument 'jdk.InitialEnvironmentVariabl' in diagnostic command.", []string{"default"}, false},
	}
	for _, tt := range tests {
		if got := jfrSettingsRefused(tt.reply, tt.settings); got != tt.want {
			t.Errorf("%s: jfrSettingsRefused() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestParseJFRCheckVerbose(t *testing.T) {
	for _, f := range jfrTwoRecordingFixtures {
		t.Run(f, func(t *testing.T) {
			recs, err := parseJFRCheckVerbose(jfrFixture(t, f))
			if err != nil {
				t.Fatal(err)
			}
			// "Recording Setting (jdk.ActiveSetting)" labels are not recordings.
			if len(recs) != 2 {
				t.Fatalf("got %d recordings, want 2: %+v", len(recs), recs)
			}
			for i, want := range []struct{ name, enabled string }{{"customer", "true"}, {"leak_repeat", "false"}} {
				r := recs[i]
				if !r.named(want.name) || r.state != "running" {
					t.Errorf("recording %d = %q (%s), want %s running", i, r.row, r.state, want.name)
				}
				for _, ev := range jfrSensitiveEvents {
					if got, listed := r.enabled[ev]; !listed || got != want.enabled {
						t.Errorf("%s: %s enabled = %q (listed %t), want %q", want.name, ev, got, listed, want.enabled)
					}
				}
			}
		})
	}

	for _, f := range jfrOneRecordingFixtures {
		t.Run(f, func(t *testing.T) {
			recs, err := parseJFRCheckVerbose(jfrFixture(t, f))
			if err != nil || len(recs) != 1 || !recs[0].named("verbose_probe") {
				t.Fatalf("parseJFRCheckVerbose() = %+v, %v; want verbose_probe alone", recs, err)
			}
		})
	}

	t.Run("jattach banner and CRLF", func(t *testing.T) {
		out := strings.Replace(jfrFixture(t, "check-verbose-one-recording-jdk8.txt"), "73:\n", "Connected to remote JVM\nJVM response code = 0\n", 1)
		out = strings.ReplaceAll(out, "\n", "\r\n")
		recs, err := parseJFRCheckVerbose(out)
		if err != nil || len(recs) != 1 || recs[0].enabled["jdk.InitialSystemProperty"] != "false" {
			t.Fatalf("parseJFRCheckVerbose() = %+v, %v", recs, err)
		}
	})

	for name, out := range map[string]string{
		// The real answers with no recordings, from jcmd (all four JDKs)
		// and jattach (the sidecar run).
		"jcmd, no recordings":                    verboseNoRecordings,
		"jattach, no recordings":                 "Connected to remote JVM\nJVM response code = 0\n\n",
		"jattach after yc's log line":            "Wed Nov 25 14:54:11 UTC 2026 INF CLI Arguments: [yc -p 58 -jCmdCaptureMode JFR.check verbose=true]\nConnected to remote JVM\nJVM response code = 0\n\n",
		"plain JFR.check's no-recordings answer": "71:\nNo available recordings.\n\nUse jcmd 71 JFR.start to start a recording.\n",
	} {
		t.Run(name, func(t *testing.T) {
			recs, err := parseJFRCheckVerbose(out)
			if err != nil || len(recs) != 0 {
				t.Fatalf("parseJFRCheckVerbose() = %+v, %v; want none", recs, err)
			}
		})
	}

	for name, out := range map[string]string{
		"header only":                   "73:\n",
		"empty":                         "",
		"JFR broken":                    "73:\nCould not initialize class jdk.jfr.internal.MetadataRepository\n",
		"malformed row":                 "73:\nRecording 1:\n",
		"jattach, JFR broken":           "Connected to remote JVM\nJVM response code = 0\nCould not initialize class jdk.jfr.internal.MetadataRepository\n",
		"text after the empty answer":   "58:\nCommand executed successfully\nsomething else\n",
		"error before the empty answer": "Error: something\nCommand executed successfully\n",
		"jattach error code":            "Connected to remote JVM\nJVM response code = 1\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			if recs, err := parseJFRCheckVerbose(out); err == nil {
				t.Errorf("parseJFRCheckVerbose() = %+v, want an error", recs)
			}
		})
	}
}

// listing builds JFR.check verbose=true output in the real format.
func listing(rows ...string) string {
	return "73:\n" + strings.Join(rows, "\n") + "\n"
}

func TestJFRSensitiveRecordings(t *testing.T) {
	for _, f := range jfrTwoRecordingFixtures {
		recs, err := parseJFRCheckVerbose(jfrFixture(t, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, skip := range []string{"", "leak_repeat"} {
			found := jfrSensitiveRecordings(recs, skip)
			if len(found) != 1 || !strings.Contains(found[0], "name=customer") || !strings.Contains(found[0], "jdk.InitialEnvironmentVariable and jdk.InitialSystemProperty turned on") {
				t.Errorf("%s, skip %q: jfrSensitiveRecordings() = %q, want the customer recording", f, skip, found)
			}
		}
		if found := jfrSensitiveRecordings(recs, "customer"); len(found) != 0 {
			t.Errorf("%s: skipping customer still found %q", f, found)
		}
	}

	for _, f := range jfrOneRecordingFixtures {
		recs, _ := parseJFRCheckVerbose(jfrFixture(t, f))
		if found := jfrSensitiveRecordings(recs, ""); len(found) != 0 {
			t.Errorf("%s: jfrSensitiveRecordings() = %q, want none", f, found)
		}
	}

	// Only stopped and closed recordings are ignored.
	two := jfrFixture(t, "check-verbose-two-recordings-jdk8.txt")
	for state, wantFound := range map[string]bool{"stopped": false, "closed": false, "delayed": true, "new": true} {
		out := strings.Replace(two, "name=customer maxsize=250.0MB (running)", "name=customer maxsize=250.0MB ("+state+")", 1)
		recs, err := parseJFRCheckVerbose(out)
		if err != nil {
			t.Fatal(err)
		}
		if found := jfrSensitiveRecordings(recs, "leak_repeat"); (len(found) > 0) != wantFound {
			t.Errorf("customer %s: jfrSensitiveRecordings() = %q, want found %t", state, found, wantFound)
		}
	}

	others := map[string]struct {
		out  string
		want string
	}{
		"one event on": {listing("Recording 1: name=other duration=60s (running)", "",
			" Initial Environment Variable (jdk.InitialEnvironmentVariable)", "   [enabled=false,period=beginChunk]",
			" Initial System Property (jdk.InitialSystemProperty)", "   [enabled=true,period=beginChunk]"),
			"jdk.InitialSystemProperty turned on"},
		// No enabled= setting: can't tell, so it counts.
		"no enabled setting": {listing("Recording 1: name=other duration=60s (running)", "",
			" Initial Environment Variable (jdk.InitialEnvironmentVariable)", "   [period=beginChunk]"),
			"jdk.InitialEnvironmentVariable turned on"},
	}
	for name, tt := range others {
		recs, err := parseJFRCheckVerbose(tt.out)
		if err != nil {
			t.Fatal(err)
		}
		if found := jfrSensitiveRecordings(recs, ""); len(found) != 1 || !strings.Contains(found[0], tt.want) {
			t.Errorf("%s: jfrSensitiveRecordings() = %q, want %q", name, found, tt.want)
		}
	}

	// Events a recording doesn't configure stay off.
	recs, _ := parseJFRCheckVerbose(listing("Recording 1: name=other duration=60s (running)", "", " CPU Load (jdk.CPULoad)", "   [enabled=true,period=1000 ms]"))
	if found := jfrSensitiveRecordings(recs, ""); len(found) != 0 {
		t.Errorf("jfrSensitiveRecordings() = %q for a recording without the events", found)
	}
}

func TestCheckRecordingExcluded(t *testing.T) {
	file := func(t *testing.T, data []byte) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "my.jfr.70.1")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	notCalled := func() (string, error) {
		t.Error("JFR.check run for a recording without the events")
		return "", nil
	}

	if err := checkRecordingExcluded("ours", file(t, jfrRecordingFixture(t, "excluded-jdk8")), notCalled); err != nil {
		t.Errorf("excluded recording: %v", err)
	}

	// The fixture's other recording started and stopped inside the window.
	leaked := file(t, jfrRecordingFixture(t, "leaked-jdk21"))
	const holds = "the recording holds 2 jdk.InitialEnvironmentVariable and 16 jdk.InitialSystemProperty events, so another JFR recording in the JVM recorded environment variables or system properties while yc's recording was running"
	stopped := listing("Recording 3: name=other duration=1s (stopped)", "",
		" Initial Environment Variable (jdk.InitialEnvironmentVariable)", "   [enabled=true,period=beginChunk]",
		"Recording 2: name=ours duration=5s (closed)", "",
		" Initial Environment Variable (jdk.InitialEnvironmentVariable)", "   [enabled=false,period=beginChunk]")
	err := checkRecordingExcluded("ours", leaked, func() (string, error) { return stopped, nil })
	if want := holds + `: "Recording 3: name=other duration=1s (stopped)" has jdk.InitialEnvironmentVariable turned on`; err == nil || err.Error() != want {
		t.Errorf("leaked, other recording listed:\n got %v\nwant %s", err, want)
	}

	// Gone from the listing, or JFR.check fails: still refused, unnamed.
	for name, check := range map[string]func() (string, error){
		"not listed":       func() (string, error) { return verboseNoRecordings, nil },
		"JFR.check failed": func() (string, error) { return "", errJVMGone },
	} {
		if err := checkRecordingExcluded("ours", leaked, check); err == nil || err.Error() != holds {
			t.Errorf("leaked, %s: %v", name, err)
		}
	}

	if err := checkRecordingExcluded("ours", file(t, []byte("not a recording")), notCalled); err == nil || !strings.Contains(err.Error(), "yc could not read the recording to check it") {
		t.Errorf("unreadable recording: %v", err)
	}
}

func TestJFRConfirmExcluded(t *testing.T) {
	for _, f := range jfrTwoRecordingFixtures {
		recs, _ := parseJFRCheckVerbose(jfrFixture(t, f))
		if err := jfrConfirmExcluded(recs, "leak_repeat"); err != nil {
			t.Errorf("%s: leak_repeat: %v, want nil", f, err)
		}
		if err := jfrConfirmExcluded(recs, "customer"); err == nil || !strings.Contains(err.Error(), "enabled=true") {
			t.Errorf("%s: customer: %v, want enabled=true", f, err)
		}
		if err := jfrConfirmExcluded(recs, "ycJFR-1-2"); err == nil || !strings.Contains(err.Error(), "does not list recording ycJFR-1-2") {
			t.Errorf("%s: absent recording: %v", f, err)
		}
	}
	for _, f := range jfrOneRecordingFixtures {
		recs, _ := parseJFRCheckVerbose(jfrFixture(t, f))
		if err := jfrConfirmExcluded(recs, "verbose_probe"); err != nil {
			t.Errorf("%s: %v, want nil", f, err)
		}
	}

	// Events not listed by these names fail.
	recs, _ := parseJFRCheckVerbose(listing("Recording 2: name=ours duration=60s (running)", "", " CPU Load (jdk.CPULoad)", "   [enabled=true,period=1000 ms]"))
	if err := jfrConfirmExcluded(recs, "ours"); err == nil || !strings.Contains(err.Error(), "does not list jdk.InitialEnvironmentVariable") {
		t.Errorf("jfrConfirmExcluded() = %v, want an unlisted-event error", err)
	}
}

func TestJFRCheckNoSensitiveRecordings(t *testing.T) {
	reply := func(out string, err error) func() (string, error) {
		return func() (string, error) { return out, err }
	}

	// The real answer with no recordings: the capture goes ahead.
	if err := checkNoSensitiveRecordings(reply(verboseNoRecordings, nil)); err != nil {
		t.Errorf("no recordings: %v", err)
	}
	if err := checkNoSensitiveRecordings(reply(jfrFixture(t, "check-verbose-one-recording-jdk21.txt"), nil)); err != nil {
		t.Errorf("only an excluding recording: %v", err)
	}

	err := checkNoSensitiveRecordings(reply(jfrFixture(t, "check-verbose-two-recordings-jdk17.txt"), nil))
	if err == nil || !strings.Contains(err.Error(), "name=customer") || !strings.Contains(err.Error(), "Stop that recording") {
		t.Errorf("customer recording running: %v", err)
	}

	if err := checkNoSensitiveRecordings(reply("73:\nCould not initialize class jdk.jfr.internal.MetadataRepository\n", nil)); err == nil {
		t.Error("unreadable listing: want an error")
	}
	if err := checkNoSensitiveRecordings(reply("", errJVMGone)); !errors.Is(err, errJVMGone) {
		t.Errorf("JVM gone: %v, want errJVMGone", err)
	}
}

func TestJFRConfirmExclusions(t *testing.T) {
	reply := func(out string) func() (string, error) {
		return func() (string, error) { return out, nil }
	}
	if err := confirmExclusions("verbose_probe", reply(jfrFixture(t, "check-verbose-one-recording-jdk8.txt"))); err != nil {
		t.Errorf("excluded, alone: %v", err)
	}
	// Excluded, but another recording has the events on.
	err := confirmExclusions("leak_repeat", reply(jfrFixture(t, "check-verbose-two-recordings-jdk21.txt")))
	if err == nil || !strings.Contains(err.Error(), "another JFR recording") {
		t.Errorf("customer recording running: %v", err)
	}
	if err := confirmExclusions("ycJFR-1-2", reply(jfrFixture(t, "check-verbose-one-recording-jdk8.txt"))); err == nil {
		t.Error("recording not listed: want an error")
	}
	failing := func() (string, error) {
		return "", errors.New("failed to run \"JFR.check verbose=true\": exit status 1")
	}
	if err := confirmExclusions("verbose_probe", failing); err == nil {
		t.Error("check failed: want an error")
	}
}

func TestWriteJFRSettingsFile(t *testing.T) {
	dir := isolateTempDir(t)

	f, err := writeJFRSettingsFile(os.Getpid(), "yc-jfr-exclusions", []byte(jfrExclusionSettings))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(f.path) != dir || !strings.HasPrefix(filepath.Base(f.path), "yc-jfr-exclusions-") || !strings.HasSuffix(f.path, ".jfc") {
		t.Errorf("path = %s, want yc-jfr-exclusions-*.jfc in %s", f.path, dir)
	}
	// Our own process shares our /tmp, so one copy only.
	if !slices.Equal(f.copies, []string{f.path}) {
		t.Errorf("copies = %q, want only %s", f.copies, f.path)
	}
	data, err := os.ReadFile(f.path)
	if err != nil || string(data) != jfrExclusionSettings {
		t.Errorf("file holds %q, %v", data, err)
	}
	if info, err := os.Stat(f.path); runtime.GOOS != "windows" && (err != nil || info.Mode().Perm() != 0o644) {
		t.Errorf("mode = %v, %v; want 0644 so the JVM's user can read it", info.Mode().Perm(), err)
	}

	f.remove()
	if left := dirEntries(t, dir); len(left) != 0 {
		t.Errorf("remove() left %q", left)
	}
}

func TestJcmdLogText(t *testing.T) {
	short := "73:\nRecording 1: name=x duration=60s (running)\n"
	if got := jcmdLogText(short); got != short {
		t.Errorf("jcmdLogText(short) = %q", got)
	}
	long := strings.Repeat("line\n", 100)
	want := strings.Repeat("line\n", jcmdLogLines) + "... (60 more lines not logged)"
	if got := jcmdLogText(long); got != want {
		t.Errorf("jcmdLogText(100 lines) = %q, want %q", got, want)
	}
}

// fakeJVM answers diagnostic commands with real-format replies and records them.
type fakeJVM struct {
	t       *testing.T
	pid     int
	verbose []string                  // successive JFR.check verbose=true replies
	starts  []func(cmd string) string // successive JFR.start replies
	check   string                    // the JFR.check reply

	sent     []string
	settings [][]string // each JFR.start's settings files, read during the call
}

var quotedSettings = regexp.MustCompile(`settings="([^"]+)"`)

func (f *fakeJVM) run(cmd string) (string, error) {
	f.sent = append(f.sent, cmd)
	switch {
	case cmd == "JFR.check verbose=true":
		if len(f.verbose) == 0 {
			f.t.Fatalf("unexpected %s", cmd)
		}
		out := f.verbose[0]
		f.verbose = f.verbose[1:]
		return out, nil
	case cmd == "JFR.check":
		return f.check, nil
	case strings.HasPrefix(cmd, "JFR.start "):
		var contents []string
		for _, m := range quotedSettings.FindAllStringSubmatch(cmd, -1) {
			data, err := os.ReadFile(m[1])
			if err != nil {
				f.t.Errorf("settings file %s missing during JFR.start: %v", m[1], err)
			}
			contents = append(contents, string(data))
		}
		f.settings = append(f.settings, contents)
		if len(f.starts) == 0 {
			f.t.Fatalf("unexpected %s", cmd)
		}
		reply := f.starts[0]
		f.starts = f.starts[1:]
		return reply(cmd), nil
	}
	f.t.Fatalf("unexpected command %q", cmd)
	return "", nil
}

func (f *fakeJVM) startCommands() []string {
	var cmds []string
	for _, c := range f.sent {
		if strings.HasPrefix(c, "JFR.start ") {
			cmds = append(cmds, c)
		}
	}
	return cmds
}

func (f *fakeJVM) started(string) string {
	return fmt.Sprintf("%d:\nStarted recording 5. The result will be written to:\n\n/tmp/my.jfr\n", f.pid)
}

// refuseFirst refuses like JDK 8-17, naming the first settings file.
func (f *fakeJVM) refuseFirst(cmd string) string {
	first := "?"
	if m := regexp.MustCompile(`settings=("([^"]+)"|(\S+))`).FindStringSubmatch(cmd); m != nil {
		first = m[2] + m[3]
	}
	return fmt.Sprintf("%d:\nCould not parse settings file '%s'\n", f.pid, first)
}

// Real replies with no recordings, from plain and verbose JFR.check.
const (
	noRecordings        = "73:\nNo available recordings.\n\nUse jcmd 73 JFR.start to start a recording.\n"
	verboseNoRecordings = "58:\nCommand executed successfully\n"
)

func TestJFRStart(t *testing.T) {
	pid := os.Getpid()
	custom := writeTestFile(t, "custom.jfc", validJFC)
	missing := filepath.Join(t.TempDir(), "nope.jfc")

	tests := []struct {
		name     string
		settings string
		exclude  bool
		verbose  []string
		starts   func(f *fakeJVM) []func(string) string
		// wantStarts: a regexp per JFR.start, after name and filename.
		wantStarts   []string
		wantSettings [][]string
		wantErr      string
		wantLog      string
	}{
		{
			name:       "both options off: the command yc has always sent",
			starts:     func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts: []string{`duration=60s$`},
		},
		{
			name:       "template",
			settings:   "profile",
			starts:     func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts: []string{`duration=60s settings=profile$`},
		},
		{
			name:         "customer file",
			settings:     custom,
			starts:       func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts:   []string{`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc"$`},
			wantSettings: [][]string{{validJFC}},
		},
		{
			name:       "missing customer file falls back to the JDK default",
			settings:   missing,
			starts:     func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts: []string{`duration=60s$`},
			wantLog:    "the file does not exist. Using the JDK's default JFR settings instead.",
		},
		{
			name:         "exclusions on the JDK default",
			exclude:      true,
			verbose:      []string{verboseNoRecordings},
			starts:       func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts:   []string{`duration=60s settings=default settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`},
			wantSettings: [][]string{{jfrExclusionSettings}},
		},
		{
			name:         "exclusions after the customer file, so they win",
			settings:     custom,
			exclude:      true,
			verbose:      []string{verboseNoRecordings},
			starts:       func(f *fakeJVM) []func(string) string { return []func(string) string{f.started} },
			wantStarts:   []string{`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc" settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`},
			wantSettings: [][]string{{validJFC, jfrExclusionSettings}},
		},
		{
			name:     "refused customer file: started again with the default",
			settings: custom,
			starts:   func(f *fakeJVM) []func(string) string { return []func(string) string{f.refuseFirst, f.started} },
			wantStarts: []string{
				`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc"$`,
				`duration=60s$`,
			},
			wantLog: "Starting the recording again with the JDK's default JFR settings.",
		},
		{
			name:     "refused customer file with exclusions: the retry keeps them",
			settings: custom,
			exclude:  true,
			verbose:  []string{verboseNoRecordings},
			starts:   func(f *fakeJVM) []func(string) string { return []func(string) string{f.refuseFirst, f.started} },
			wantStarts: []string{
				`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc" settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`,
				`duration=60s settings=default settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`,
			},
		},
		{
			// Never retried without the exclusions.
			name:       "refused exclusions: no JFR",
			exclude:    true,
			verbose:    []string{verboseNoRecordings},
			starts:     func(f *fakeJVM) []func(string) string { return []func(string) string{f.refuseFirst} },
			wantStarts: []string{`duration=60s settings=default settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`},
			wantErr:    "skipping JFR capture: excludeSensitiveData is on, but the JVM refused the settings file that turns off jdk.InitialEnvironmentVariable and jdk.InitialSystemProperty",
		},
		{
			name:     "refused twice with exclusions: no JFR",
			settings: custom,
			exclude:  true,
			verbose:  []string{verboseNoRecordings},
			starts:   func(f *fakeJVM) []func(string) string { return []func(string) string{f.refuseFirst, f.refuseFirst} },
			wantStarts: []string{
				`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc" settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`,
				`duration=60s settings=default settings="[^"]*yc-jfr-exclusions-[^"]*\.jfc"$`,
			},
			wantErr: "skipping JFR capture: excludeSensitiveData is on, but the JVM refused the settings file",
		},
		{
			// Only settings refusals are retried.
			name:     "other refusals are not retried",
			settings: custom,
			starts: func(f *fakeJVM) []func(string) string {
				return []func(string) string{func(string) string { return fmt.Sprintf("%d:\nJava heap space\n", f.pid) }}
			},
			wantStarts: []string{`duration=60s settings="[^"]*yc-jfr-settings-[^"]*\.jfc"$`},
			wantErr:    `the JVM answered JFR.start with "Java heap space"`,
		},
		{
			name:    "another recording records the events: no JFR",
			exclude: true,
			verbose: []string{jfrFixture(t, "check-verbose-two-recordings-jdk17.txt")},
			wantErr: "skipping JFR capture: excludeSensitiveData is on, but another JFR recording in the JVM records environment variables or system properties: \"Recording 17: name=customer maxsize=250.0MB (running)\" has jdk.InitialEnvironmentVariable and jdk.InitialSystemProperty turned on",
		},
		{
			name:    "other recordings can't be read: no JFR",
			exclude: true,
			verbose: []string{"73:\nCould not initialize class jdk.jfr.internal.MetadataRepository\n"},
			wantErr: "skipping JFR capture: excludeSensitiveData is on, but could not read the JVM's other JFR recordings",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := isolateTempDir(t)
			logs := captureLogs(t)
			f := &fakeJVM{t: t, pid: pid, verbose: tt.verbose, check: noRecordings}
			if tt.starts != nil {
				f.starts = tt.starts(f)
			}
			j := &JFR{Pid: pid, Settings: tt.settings, ExcludeSensitiveData: tt.exclude, jcmd: f.run}

			name, jvmPath, err := j.start(60 * time.Second)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("start() error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatalf("start() error = %v", err)
			}

			starts := f.startCommands()
			if len(starts) != len(tt.wantStarts) {
				t.Fatalf("sent %d JFR.start, want %d: %q", len(starts), len(tt.wantStarts), starts)
			}
			head := regexp.QuoteMeta(fmt.Sprintf("JFR.start name=\"ycJFR-%d-", pid)) + `\d+" filename="` + regexp.QuoteMeta(filepath.Join(tmp, fmt.Sprintf("my.jfr.%d.", pid))) + `\d+" `
			for i, want := range tt.wantStarts {
				if !regexp.MustCompile("^" + head + want).MatchString(starts[i]) {
					t.Errorf("JFR.start %d:\n got %s\nwant %s%s", i+1, starts[i], head, want)
				}
			}
			if tt.wantSettings != nil && !slices.EqualFunc(f.settings, tt.wantSettings, slices.Equal[[]string]) {
				t.Errorf("settings files held %q, want %q", f.settings, tt.wantSettings)
			}
			if tt.exclude {
				for _, c := range starts {
					if !strings.Contains(c, "yc-jfr-exclusions-") {
						t.Errorf("JFR.start without the exclusions: %s", c)
					}
				}
			}

			// The returned recording is the last JFR.start's.
			if err == nil {
				last := starts[len(starts)-1]
				if !strings.Contains(last, `name="`+name+`"`) || !strings.Contains(last, `filename="`+jvmPath+`"`) {
					t.Errorf("start() = %s, %s; not the last JFR.start: %s", name, jvmPath, last)
				}
			}
			// Settings files are removed after JFR.start.
			if left := dirEntries(t, tmp); len(left) != 0 {
				t.Errorf("left in the temp directory: %q", left)
			}
			if tt.wantLog != "" && !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("logs lack %q: %s", tt.wantLog, logs)
			}
		})
	}
}

func TestJFRSettleUnconfirmedStartNotListed(t *testing.T) {
	pid := os.Getpid()
	jvmPath := filepath.Join(t.TempDir(), "my.jfr")
	tests := map[string]struct {
		out  string
		err  error
		want bool
	}{
		"confirmed absent": {out: noRecordings, want: true},
		"never started":    {out: "73:\nRecording 1: name=ycJFR-1-2 duration=60s (new)\n"},
		"check failed":     {err: errors.New("exit status 1")},
		"JVM gone":         {err: errJVMGone},
	}
	for name, tt := range tests {
		err := settleUnconfirmedStart(pid, "ycJFR-1-2", jvmPath, "Could not parse settings file 'default'", func() (string, error) { return tt.out, tt.err })
		var notListed *jfrNotListedError
		if got := errors.As(err, &notListed); got != tt.want {
			t.Errorf("%s: not-listed = %t, want %t (err %v)", name, got, tt.want, err)
		}
	}
}
