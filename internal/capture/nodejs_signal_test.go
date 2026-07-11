//go:build !windows

package capture

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestParseNodeSignal(t *testing.T) {
	cases := []struct {
		name    string
		want    syscall.Signal
		wantErr bool
	}{
		{"SIGUSR2", syscall.SIGUSR2, false},
		{"usr2", syscall.SIGUSR2, false},
		{"SIGQUIT", syscall.SIGQUIT, false},
		{"SIGUSR1", 0, true}, // refused: activates the inspector
		{"USR1", 0, true},
		{"SIGKILL", 0, true}, // not in the accepted table
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := parseNodeSignal(c.name)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseNodeSignal(%q) expected error, got signal %v", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseNodeSignal(%q) unexpected error: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseNodeSignal(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseNodeReportSettingsFromArgs(t *testing.T) {
	settings := parseNodeReportSettingsFromArgs([]string{
		"node",
		"--report-directory=reports",
		"--report-filename=threaddump-signalmode.out",
	}, "/srv/app")
	if settings.dir != filepath.Join("/srv/app", "reports") {
		t.Errorf("dir = %q, want /srv/app/reports", settings.dir)
	}
	if settings.filename != "threaddump-signalmode.out" {
		t.Errorf("filename = %q, want threaddump-signalmode.out", settings.filename)
	}

	settings = parseNodeReportSettingsFromArgs([]string{
		"node",
		"--report-dir",
		"/var/reports",
		"--report-filename",
		"report.out",
	}, "/srv/app")
	if settings.dir != "/var/reports" {
		t.Errorf("spaced dir = %q, want /var/reports", settings.dir)
	}
	if settings.filename != "report.out" {
		t.Errorf("spaced filename = %q, want report.out", settings.filename)
	}
}

func TestNodeReportFilesIncludesCustomFilename(t *testing.T) {
	dir := t.TempDir()
	defaultPath := filepath.Join(dir, "report.20260709.123456.123.001.json")
	customPath := filepath.Join(dir, "threaddump-signalmode.out")
	if err := os.WriteFile(defaultPath, []byte(`{"default":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customPath, []byte(`{"custom":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	files := nodeReportFiles(nodeReportSettings{dir: dir, filename: "threaddump-signalmode.out"})
	if _, ok := files[defaultPath]; !ok {
		t.Errorf("default report file was not discovered")
	}
	if _, ok := files[customPath]; !ok {
		t.Errorf("custom report filename was not discovered")
	}
}
