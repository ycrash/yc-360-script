package capture

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/config"
)

// silentServer accepts connections and never answers, so a connect waits until its
// context ends.
func silentServer(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var (
		mu       sync.Mutex
		accepted []net.Conn
	)

	t.Cleanup(func() {
		listener.Close()

		mu.Lock()
		defer mu.Unlock()

		for _, conn := range accepted {
			conn.Close()
		}
	})

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			mu.Lock()
			accepted = append(accepted, conn)
			mu.Unlock()
		}
	}()

	return listener.Addr().(*net.TCPAddr).Port
}

// assertTheRunStopsTheWindow runs a two-minute window against a server that never
// answers, ends its run's context before Run or mid-window, and checks the window
// ends at once and its files say so.
func assertTheRunStopsTheWindow(t *testing.T, run func(context.Context) (Result, error), artifact string) {
	for _, tt := range []struct {
		name   string
		before bool
	}{
		{"mid-window", false},
		{"before Run, which Kill cannot reach", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			chdirToCaptureDir(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tt.before {
				cancel()
			}

			done := make(chan struct{})

			go func() {
				defer close(done)

				_, err := run(ctx)
				assert.NoError(t, err)
			}()

			if !tt.before {
				time.Sleep(250 * time.Millisecond)

				select {
				case <-done:
					t.Fatal("Run finished before the run ended; the test proves nothing")
				default:
				}

				cancel()
			}

			started := time.Now()

			select {
			case <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the run's end did not stop the window")
			}

			assert.Less(t, time.Since(started), 4*time.Second,
				"the window ran on to ConnectTimeout or its end")
			assert.Contains(t, readSampledArtifact(t, artifact), "samples_written=0")
		})
	}
}

func TestMySQLCaptureStopsWhenItsRunEnds(t *testing.T) {
	assertTheRunStopsTheWindow(t, func(ctx context.Context) (Result, error) {
		window := config.Duration(2 * time.Minute)
		task := &MySQLCapture{
			Target: &config.MySQL{DBTarget: config.DBTarget{
				ID: "orders-primary", Host: "127.0.0.1", Port: silentServer(t), Username: "ycrash_monitor",
				CaptureDuration: &window, TLS: &config.TargetTLS{Enabled: new(bool)},
			}},
			RunCtx: ctx,
		}

		return task.Run()
	}, "my_sessions.txt")
}

func TestPostgresCaptureStopsWhenItsRunEnds(t *testing.T) {
	assertTheRunStopsTheWindow(t, func(ctx context.Context) (Result, error) {
		target := withWindow(t, 2*time.Minute)
		target.Port = silentServer(t)
		target.TLS = &config.PostgresTLS{Enabled: new(bool)}

		task := &PostgresCapture{Target: target, RunCtx: ctx}

		return task.Run()
	}, PostgresBloatFileName)
}
