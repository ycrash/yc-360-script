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

		assert.Equal(t, expected, strings.Count(readCapture(t, file.Name()), "snapshot"),
			"an application capture takes the number of snapshots it always has")
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

// df and dmesg are the two host files whose format has only ever held one
// reading, and they carry the same dt whichever kind of capture wrote them. A
// database capture takes them exactly as an application capture does - the
// command's output and nothing else - so one dt never means two shapes.
func TestDiskAndDMesgTakeOneUnadornedReading(t *testing.T) {
	originalDisk, originalDMesg := executils.Disk, executils.DMesg
	t.Cleanup(func() { executils.Disk, executils.DMesg = originalDisk, originalDMesg })

	executils.Disk = executils.Command{"echo", "reading"}
	executils.DMesg = executils.Command{"echo", "reading"}

	t.Run("disk.out", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&Disk{}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, "reading\n", readCapture(t, file.Name()),
			"no header, no separator, no second block")
	})

	t.Run("dmesg.out", func(t *testing.T) {
		t.Chdir(t.TempDir())

		file, err := (&DMesg{}).CaptureToFile()
		require.NoError(t, err)
		defer file.Close()

		assert.Equal(t, "reading\n", readCapture(t, file.Name()),
			"no header, no separator, no second block")
	})
}
