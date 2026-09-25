//go:build unix

package capture

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsRegularFile(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(regular, []byte("2026-09-25 INFO started\n"), 0644))

	fifo := filepath.Join(dir, "pipe.log")
	require.NoError(t, syscall.Mkfifo(fifo, 0644))

	assert.True(t, isRegularFile(regular))
	assert.False(t, isRegularFile(fifo), "a FIFO must not be sampled")
	assert.False(t, isRegularFile(dir), "a directory must not be sampled")
	assert.False(t, isRegularFile(filepath.Join(dir, "missing.log")))
}

func TestDiscoverOpenedLogFilesByProcess_FIFO(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("DiscoverOpenedLogFilesByProcess is verified through /proc on Linux only")
	}

	dir := t.TempDir()

	// Hold the FIFO open without a writer; O_NONBLOCK lets the open succeed.
	fifoPath := filepath.Join(dir, "pipe.log")
	require.NoError(t, syscall.Mkfifo(fifoPath, 0644))
	fifo, err := os.OpenFile(fifoPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	require.NoError(t, err)
	defer fifo.Close()

	// Discovering this log proves discovery got past the FIFO.
	logPath := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(logPath, []byte("2026-09-25 INFO started\n"), 0644))
	logFile, err := os.Open(logPath)
	require.NoError(t, err)
	defer logFile.Close()

	var discovered []string
	done := make(chan error, 1)
	go func() {
		var err error
		discovered, err = DiscoverOpenedLogFilesByProcess(os.Getpid())
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("DiscoverOpenedLogFilesByProcess blocked on the FIFO")
	}

	assert.NotContains(t, discovered, fifoPath, "a FIFO must not be discovered as a log")
	assert.Contains(t, discovered, logPath, "the regular log opened after the FIFO must still be discovered")
}
