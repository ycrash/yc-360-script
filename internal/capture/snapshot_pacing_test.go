package capture

import (
	"os"
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

func TestDiskSecondReading(t *testing.T) {
	original := executils.Disk
	t.Cleanup(func() { executils.Disk = original })
	executils.Disk = executils.Command{"echo", "reading"}

	tests := []struct {
		name  string
		gap   time.Duration
		stop  <-chan struct{}
		want  int
		stamp bool
	}{
		{
			name: "no gap keeps the single reading every application capture takes",
			want: 1,
		},
		{
			name:  "a gap appends a second, timestamped reading",
			gap:   time.Millisecond,
			want:  2,
			stamp: true,
		},
		{
			name: "a torn-down run uploads the reading it already has",
			gap:  time.Hour,
			stop: closedStop(),
			want: 1,
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
			assert.Equal(t, tt.stamp, strings.Contains(body, "\n\n"),
				"the separator is written only where there is a second reading, so "+
					"single-reading output stays as it was")
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

		assert.Equal(t, 2, strings.Count(readCapture(t, file.Name()), "primary"))
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
