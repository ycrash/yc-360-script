package capture

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"yc-agent/internal/capture/executils"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func closedStop() <-chan struct{} {
	stop := make(chan struct{})
	close(stop)

	return stop
}

func readCapture(t *testing.T, name string) string {
	t.Helper()

	body, err := os.ReadFile(name)
	require.NoError(t, err)

	return string(body)
}

// readingHeader matches the timestamp line netstat.out and ps.out write before
// every reading; a file with more than one reading must label all of them.
var readingHeader = regexp.MustCompile(`(?m)^[A-Z][a-z]{2} [A-Z][a-z]{2} +\d+ [\d:]+ .* \d{4} *$`)

func countReadingHeaders(body string) int {
	return len(readingHeader.FindAllString(body, -1))
}

func TestDiskSecondReading(t *testing.T) {
	original := executils.Disk
	t.Cleanup(func() { executils.Disk = original })
	executils.Disk = executils.Command{"echo", "reading"}

	tests := []struct {
		name    string
		gap     time.Duration
		stop    <-chan struct{}
		want    int
		headers int
	}{
		{
			name: "no gap keeps the single reading every application capture takes",
			want: 1,
			// No header at all: an application bundle's disk.out must stay
			// byte-for-byte what it has always been.
			headers: 0,
		},
		{
			name:    "a gap appends a second reading, and both carry a header",
			gap:     time.Millisecond,
			want:    2,
			headers: 2,
		},
		{
			name:    "a torn-down run uploads the reading it already has",
			gap:     time.Hour,
			stop:    closedStop(),
			want:    1,
			headers: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())

			d := &Disk{sleepBetweenCaptures: tt.gap, stop: tt.stop}

			file, err := d.CaptureToFile()
			require.NoError(t, err)
			defer file.Close()

			require.NoError(t, d.captureSecondReading(file))

			body := readCapture(t, file.Name())

			assert.Equal(t, tt.want, strings.Count(body, "reading"))
			assert.Equal(t, tt.headers, countReadingHeaders(body),
				"a multi-reading file labels every reading, the way netstat.out does; "+
					"a single-reading file carries no header at all")
		})
	}
}

func TestDMesgSecondReading(t *testing.T) {
	originalPrimary, originalFallback := executils.DMesg, executils.DMesg2
	t.Cleanup(func() { executils.DMesg, executils.DMesg2 = originalPrimary, originalFallback })

	t.Run("a gap appends a second reading from the command that worked", func(t *testing.T) {
		t.Chdir(t.TempDir())

		executils.DMesg = executils.Command{"echo", "primary"}
		executils.DMesg2 = executils.Command{"echo", "fallback"}

		d := &DMesg{sleepBetweenCaptures: time.Millisecond}

		file, err := d.CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		require.NoError(t, d.captureSecondReading(file))

		body := readCapture(t, file.Name())

		assert.Equal(t, 2, strings.Count(body, "primary"))
		assert.Equal(t, 2, countReadingHeaders(body), "both readings are labelled")
	})

	t.Run("the second reading replays the fallback rather than the failing command", func(t *testing.T) {
		t.Chdir(t.TempDir())

		executils.DMesg = executils.Command{"echo", "primary"}
		executils.DMesg2 = executils.Command{"echo", "fallback"}

		d := &DMesg{sleepBetweenCaptures: time.Millisecond, usedFallback: true}

		file, err := os.Create(dmesgOutputPath)
		require.NoError(t, err)
		defer file.Close()

		require.NoError(t, d.captureSecondReading(file))

		body := readCapture(t, file.Name())

		assert.Contains(t, body, "fallback")
		assert.NotContains(t, body, "primary",
			"retrying the command already known to fail would write an error message as data")
	})

	t.Run("the second reading never resets the first", func(t *testing.T) {
		t.Chdir(t.TempDir())

		executils.DMesg = executils.Command{"echo", "primary"}
		executils.DMesg2 = executils.Command{"echo", "fallback"}

		d := &DMesg{sleepBetweenCaptures: time.Hour, stop: closedStop()}

		file, err := d.CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		require.NoError(t, d.captureSecondReading(file))

		assert.Equal(t, 1, strings.Count(readCapture(t, file.Name()), "primary"))
	})

	t.Run("the fallback's output is labelled too, after the reset drops the header", func(t *testing.T) {
		t.Chdir(t.TempDir())

		executils.DMesg = executils.Command{"echo", "primary"}
		executils.DMesg2 = executils.Command{"echo", "fallback"}

		d := &DMesg{sleepBetweenCaptures: time.Millisecond}

		file, err := os.Create(dmesgOutputPath)
		require.NoError(t, err)
		defer file.Close()

		require.NoError(t, d.writeReadingHeader(file))
		require.NoError(t, d.resetFile(file))

		assert.Equal(t, 1, countReadingHeaders(readCapture(t, file.Name())),
			"the truncate removed the header, so resetFile puts it back")
	})
}

func TestApplicationCaptureOutputIsUnchanged(t *testing.T) {
	originalDisk, originalDMesg := executils.Disk, executils.DMesg
	t.Cleanup(func() { executils.Disk, executils.DMesg = originalDisk, originalDMesg })

	executils.Disk = executils.Command{"echo", "reading"}
	executils.DMesg = executils.Command{"echo", "reading"}

	// An application capture constructs these with no gap, and their files are a
	// shared format: the same dt, the same receiver, whichever kind of capture
	// produced them. A second reading is the database capture's alone.
	t.Run("disk.out", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&Disk{}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, "reading\n", readCapture(t, file.Name()))
	})

	t.Run("dmesg.out", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&DMesg{}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, "reading\n", readCapture(t, file.Name()))
	})
}

func TestPSPacing(t *testing.T) {
	originalPS, originalPS2 := executils.PS, executils.PS2
	t.Cleanup(func() { executils.PS, executils.PS2 = originalPS, originalPS2 })

	executils.PS = executils.Command{"echo", "snapshot"}
	executils.PS2 = nil

	expected := executils.SCRIPT_SPAN/executils.JAVACORE_INTERVAL - 1

	t.Run("no gap keeps every snapshot, back to back", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&PS{}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, expected, strings.Count(readCapture(t, file.Name()), "snapshot"))
	})

	t.Run("a gap paces them and a torn-down run stops early", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&PS{sleepBetweenCaptures: time.Hour, stop: closedStop()}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, 1, strings.Count(readCapture(t, file.Name()), "snapshot"),
			"the first snapshot is taken before any gap, so it always survives")
	})
}

func TestNetStatStopsBeforeItsSecondSnapshot(t *testing.T) {
	original := executils.NetState
	t.Cleanup(func() { executils.NetState = original })
	executils.NetState = executils.Command{"echo", "connections"}

	t.Chdir(t.TempDir())

	ns := &NetStat{sleepBetweenCaptures: time.Hour, stop: closedStop()}

	_, err := ns.Run()
	require.NoError(t, err)

	assert.Equal(t, 1, strings.Count(readCapture(t, netStatOutputPath), "connections"))
}
