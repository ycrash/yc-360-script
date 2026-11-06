package ondemand

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAttachDisabledByFlag(t *testing.T) {
	cases := map[string]bool{
		`"C:\jdk\bin\java.exe" -XX:+DisableAttachMechanism -jar app.jar`:            true,
		"/usr/bin/java -Xmx1g -XX:+DisableAttachMechanism -cp app.jar Main":         true,
		"java -XX:+DisableAttachMechanism -XX:-DisableAttachMechanism -jar app.jar": false, // last one wins
		"java -XX:-DisableAttachMechanism -XX:+DisableAttachMechanism -jar app.jar": true,
		"java -Xmx1g -jar app.jar": false,
		"":                         false,
	}
	for cmdline, want := range cases {
		assert.Equal(t, want, attachDisabledByFlag(cmdline), cmdline)
	}
}

func TestAttachCaptureNames(t *testing.T) {
	cases := []struct {
		name string
		opts attachCaptureOptions
		want []string
	}{
		{"defaults", attachCaptureOptions{jfr: true},
			[]string{"thread dumps", "JFR", "class histogram", "system properties"}},
		{"-hd", attachCaptureOptions{jfr: true, heapDump: true},
			[]string{"thread dumps", "JFR", "heap dump", "class histogram", "system properties"}},
		{"-hd with -hdPath reads the file", attachCaptureOptions{heapDump: true, hdPath: "heap.hprof"},
			[]string{"thread dumps", "class histogram", "system properties"}},
		{"-tdPath reads the file", attachCaptureOptions{tdPath: "td.txt", jfr: true},
			[]string{"JFR", "class histogram", "system properties"}},
		{"-minimalTouch", attachCaptureOptions{jfr: true, heapDump: true, minimalTouch: true},
			[]string{"thread dumps"}},
		{"nothing attaches", attachCaptureOptions{tdPath: "td.txt", minimalTouch: true}, nil},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, attachCaptureNames(c.opts), c.name)
	}
}

func TestAttachDisabledMessage(t *testing.T) {
	assert.Equal(t, "PID 308856 was started with -XX:+DisableAttachMechanism, so the yc-360 script can't attach to it: "+
		"thread dumps, JFR, class histogram and system properties will fail. GC data and host data are still captured. "+
		"To capture everything, remove -XX:+DisableAttachMechanism and restart the application.",
		attachDisabledMessage(308856, []string{"thread dumps", "JFR", "class histogram", "system properties"}, false))

	assert.Equal(t, "PID 1 was started with -XX:+DisableAttachMechanism, so the yc-360 script can't attach to it: "+
		"thread dumps will fail. Host data, and GC data from the JVM's GC log file if it writes one, are still captured. "+
		"To capture everything, remove -XX:+DisableAttachMechanism and restart the application.",
		attachDisabledMessage(1, []string{"thread dumps"}, true))
}

func TestWarnIfAttachDisabled(t *testing.T) {
	original := readAttachCmdline
	t.Cleanup(func() { readAttachCmdline = original })

	reads := 0
	readAttachCmdline = func(int) ([]byte, error) { reads++; return nil, errors.New("access denied") }
	warnIfAttachDisabled(42, attachCaptureOptions{}) // a read error is ignored
	assert.Equal(t, 1, reads)

	// When nothing in the run attaches, the command line isn't even read.
	warnIfAttachDisabled(42, attachCaptureOptions{tdPath: "td.txt", minimalTouch: true})
	assert.Equal(t, 1, reads)

	readAttachCmdline = func(int) ([]byte, error) { return []byte("java -XX:+DisableAttachMechanism -jar a.jar"), nil }
	warnIfAttachDisabled(42, attachCaptureOptions{jfr: true})
}
