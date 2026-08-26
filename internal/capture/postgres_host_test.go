package capture

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHostSpan(t *testing.T) {
	assert.Equal(t, 115*time.Second, hostSpan(120*time.Second),
		"the closing reading lands inside the window, not after the artifacts have closed")
	assert.Equal(t, 595*time.Second, hostSpan(600*time.Second))
	assert.Equal(t, time.Second, hostSpan(2*time.Second),
		"a window shorter than the margin still gets two readings")
	assert.Equal(t, time.Second, hostSpan(0))
}

func TestSnapshotGapElapsed(t *testing.T) {
	assert.True(t, snapshotGapElapsed(0, nil), "a zero gap is the back-to-back shape, not a skip")
	assert.True(t, snapshotGapElapsed(time.Millisecond, nil))

	stop := make(chan struct{})
	close(stop)

	assert.False(t, snapshotGapElapsed(time.Hour, stop),
		"a torn-down run does not wait out a reading nobody will collect")
	assert.True(t, snapshotGapElapsed(0, stop),
		"a zero gap has nothing to cut short")
}

func TestSnapshotGapElapsedReturnsWhenStopClosesMidWait(t *testing.T) {
	stop := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(stop)
	}()

	start := time.Now()
	assert.False(t, snapshotGapElapsed(time.Minute, stop))
	assert.Less(t, time.Since(start), 10*time.Second)
}

func TestWaitForHostCapture(t *testing.T) {
	failed := make(chan Result, 1)
	failed <- Result{Msg: "upload refused", Ok: false}

	succeeded := make(chan Result, 1)
	succeeded <- Result{Msg: "uploaded", Ok: true}

	messages, ok := waitForHostCapture([]hostCollector{
		{"df", succeeded},
		{"dmesg", failed},
	})

	assert.Equal(t, []string{"df: uploaded", "dmesg: upload refused"}, messages)
	assert.False(t, ok, "one refused upload makes the run not ok")
}

func TestWaitForHostCaptureOnASkippedGate(t *testing.T) {
	messages, ok := waitForHostCapture(nil)

	assert.Empty(t, messages)
	assert.True(t, ok, "a gate that skipped on purpose is not a failure")
}

func TestWithHostCapture(t *testing.T) {
	base := Result{Msg: "pg_metadata.txt written", Ok: true}

	assert.Equal(t, base, withHostCapture(base, nil, true),
		"a database-only bundle reads exactly as it did before the gate existed")

	got := withHostCapture(base, []string{"df: uploaded"}, false)

	assert.Equal(t, "pg_metadata.txt written | df: uploaded", got.Msg)
	assert.False(t, got.Ok)
}

func TestWithHostCaptureKeepsAFailedDatabaseCaptureFailed(t *testing.T) {
	got := withHostCapture(Result{Msg: "pg upload refused", Ok: false}, []string{"df: uploaded"}, true)

	require.False(t, got.Ok)
}
