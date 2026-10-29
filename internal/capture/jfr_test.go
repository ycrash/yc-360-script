package capture

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"yc-agent/internal/logger"
)

// Real jcmd/jattach replies from Temurin 8u504, 11 and 17.
func TestJFRStartedReply(t *testing.T) {
	started := []string{
		"1784893:\nStarted recording 1. The result will be written to:\n\n/tmp/my.jfr.1784893.1793200608445870574\n",
		"Connected to remote JVM\nJVM response code = 0\nStarted recording 1. The result will be written to:\n\n/tmp/my.jfr.1784893.1793200608445870574\n\n",
	}
	refused := []string{
		"1784893:\nJava heap space\n",
		"Connected to remote JVM\nJVM response code = 0\nJava heap space\n\n",
		"1784893:\nCould not initialize class jdk.jfr.internal.MetadataRepository\n",
		"1784893:\n",
	}

	for _, out := range started {
		if !strings.Contains(out, jfrStartedReply) {
			t.Errorf("%q not recognised as a started recording", out)
		}
	}
	for _, out := range refused {
		if strings.Contains(out, jfrStartedReply) {
			t.Errorf("%q mistaken for a started recording", out)
		}
	}
}

func TestJFRReply(t *testing.T) {
	tests := []struct{ out, want string }{
		{"1784893:\nJava heap space\n", "Java heap space"},
		{"Connected to remote JVM\nJVM response code = 0\nJava heap space\n\n", "Connected to remote JVM\nJVM response code = 0\nJava heap space"},
		{"1784893:\n", ""},
		{"", ""},
	}
	for _, tt := range tests {
		if got := jcmdReply(1784893, tt.out); got != tt.want {
			t.Errorf("jcmdReply(%q) = %q, want %q", tt.out, got, tt.want)
		}
	}
}

func TestJFRRecordingState(t *testing.T) {
	const name = "ycJFR-4321-17881567889427012"

	tests := []struct {
		name    string
		out     string
		want    string
		wantErr bool
	}{
		{name: "running", out: "4321:\nRecording 1: name=" + name + " duration=1m (running)\n", want: "running"},
		{name: "never started", out: "4321:\nRecording 1: name=" + name + " duration=1m (new)\n", want: "new"},
		{name: "closed", out: "4321:\nRecording 1: name=" + name + " duration=1m (closed)\n", want: "closed"},
		{name: "quoted name", out: "4321:\nRecording: recording=1 name=\"" + name + "\" duration=1m (stopped)\n", want: "stopped"},
		{name: "filename holds a state", out: "4321:\nRecording: recording=1 name=\"" + name + "\" duration=1m filename=\"/tmp/(new)/my.jfr\" compress=false (running)\n", want: "running"},
		{name: "capitalised state", out: "4321:\nRecording 1: name=" + name + " duration=1m (Running)\n", want: "running"},
		{name: "unrecognised state", out: "4321:\nRecording 1: name=" + name + " duration=1m (paused)\n", want: "unknown"},
		{name: "malformed row beside ours", out: "4321:\nRecording 1:\nRecording 2: name=" + name + " duration=1m (running)\n", want: "running"},
		{name: "no recordings", out: "4321:\nNo available recordings.\n\nUse jcmd 4321 JFR.start to start a recording.\n", want: ""},
		{name: "only a longer name", out: "4321:\nRecording 1: name=" + name + "0 duration=1m (running)\n", want: ""},
		{name: "only a shorter name", out: "4321:\nRecording 1: name=" + name[:len(name)-1] + " duration=1m (running)\n", want: ""},
		// Malformed rows could hide the recording, so they aren't "absent".
		{name: "row with only an id", out: "4321:\nRecording 1:\n", wantErr: true},
		{name: "row without a state", out: "4321:\nRecording 1: name=" + name + " duration=1m\n", wantErr: true},
		{name: "other row malformed", out: "4321:\nRecording 1: name=other duration=1m\n", wantErr: true},
		{name: "row without a name", out: "4321:\nRecording 1: duration=1m (running)\n", wantErr: true},
		{name: "JFR broken", out: "4321:\nCould not initialize class jdk.jfr.internal.MetadataRepository\n", wantErr: true},
		{name: "header only", out: "4321:\n", wantErr: true},
		{name: "empty", out: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := jfrRecordingState(tt.out, name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("jfrRecordingState() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("jfrRecordingState() = %q, want %q", got, tt.want)
			}
		})
	}
}

// One JFR.check decides: carry on or fail, and keep or remove the file.
func TestJFRSettleUnconfirmedStart(t *testing.T) {
	const name = "ycJFR-1784893-1793200608445870574"
	const reply = "Java heap space"
	pid := os.Getpid()
	listing := func(body string) string { return fmt.Sprintf("%d:\n%s\n", pid, body) }

	tests := []struct {
		name         string
		checkOut     string
		checkErr     error
		data         []byte // contents of the file JFR.start created
		wantContinue bool
		wantKept     bool
	}{
		{
			name:         "running despite the error reply",
			checkOut:     listing("Recording 1: name=" + name + " duration=10s (running)"),
			wantContinue: true,
			wantKept:     true,
		},
		{
			name:         "running, filename holds (new)",
			checkOut:     listing("Recording: recording=1 name=\"" + name + "\" duration=10s filename=\"/tmp/(new)/my.jfr\" compress=false (running)"),
			wantContinue: true,
			wantKept:     true,
		},
		{
			name:     "malformed row",
			checkOut: listing("Recording 1:"),
			wantKept: true,
		},
		{
			name:     "created but never started",
			checkOut: listing("Recording 1: name=" + name + " duration=10s (new)"),
		},
		{
			name:     "never started, file has data",
			checkOut: listing("Recording 1: name=" + name + " duration=10s (new)"),
			data:     []byte("FLR\x00"),
			wantKept: true,
		},
		{
			name:     "confirmed absent",
			checkOut: listing("No available recordings.\n\nUse jcmd 1 JFR.start to start a recording."),
		},
		{
			name:     "absent, a longer name is running",
			checkOut: listing("Recording 1: name=" + name + "0 duration=10s (running)"),
		},
		{
			name:     "stopped",
			checkOut: listing("Recording 1: name=" + name + " duration=10s (stopped)"),
			wantKept: true,
		},
		{
			name:     "not a listing",
			checkOut: listing("Could not initialize class jdk.jfr.internal.MetadataRepository"),
			wantKept: true,
		},
		{
			name:     "empty output",
			checkOut: "",
			wantKept: true,
		},
		{
			name:     "check failed",
			checkErr: errors.New("failed to run \"JFR.check\": exit status 1"),
			wantKept: true,
		},
		{
			name:     "JVM gone",
			checkErr: errJVMGone,
		},
		{
			name:     "JVM gone, file has data",
			checkErr: errJVMGone,
			data:     []byte("FLR\x00"),
			wantKept: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			l := zerolog.New(&logs)
			saved := logger.GetLogger()
			logger.SetLogger(&l)
			t.Cleanup(func() { logger.SetLogger(saved) })

			jvmPath := filepath.Join(t.TempDir(), "my.jfr.1784893.1793200608445870574")
			if err := os.WriteFile(jvmPath, tt.data, 0644); err != nil {
				t.Fatal(err)
			}

			checks := 0
			err := settleUnconfirmedStart(pid, name, jvmPath, reply, func() (string, error) {
				checks++
				return tt.checkOut, tt.checkErr
			})

			if checks != 1 {
				t.Errorf("JFR.check ran %d times, want 1", checks)
			}
			if tt.wantContinue {
				if err != nil {
					t.Errorf("settleUnconfirmedStart() = %v, want nil (carry on)", err)
				}
				// The warning must quote the JVM's reply.
				if !strings.Contains(logs.String(), `JFR.start returned \"Java heap space\", but JFR.check confirmed recording `+name+` is running`) {
					t.Errorf("warning does not carry the JVM's reply; logs: %s", logs.String())
				}
			} else {
				if err == nil {
					t.Fatal("settleUnconfirmedStart() = nil, want a failure")
				}
				if !strings.Contains(err.Error(), `answered JFR.start with "Java heap space"`) {
					t.Errorf("settleUnconfirmedStart() = %q, want it to quote the JVM's reply", err)
				}
			}

			_, statErr := os.Stat(jvmPath)
			if kept := statErr == nil; kept != tt.wantKept {
				t.Errorf("file kept = %v, want %v (stat err = %v)", kept, tt.wantKept, statErr)
			}
		})
	}
}

func TestJFRRemoveUnstartedRecording(t *testing.T) {
	dir := t.TempDir()

	t.Run("empty stub is removed", func(t *testing.T) {
		p := filepath.Join(dir, "stub.jfr")
		if err := os.WriteFile(p, nil, 0644); err != nil {
			t.Fatal(err)
		}
		removeUnstartedRecording(os.Getpid(), p)
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("empty stub %s still exists (stat err = %v)", p, err)
		}
	})

	t.Run("file with data is kept", func(t *testing.T) {
		p := filepath.Join(dir, "data.jfr")
		if err := os.WriteFile(p, []byte("FLR\x00"), 0644); err != nil {
			t.Fatal(err)
		}
		removeUnstartedRecording(os.Getpid(), p)
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file with data was removed: %v", err)
		}
	})

	t.Run("missing file is a no-op", func(t *testing.T) {
		removeUnstartedRecording(os.Getpid(), filepath.Join(dir, "missing.jfr"))
	})
}
