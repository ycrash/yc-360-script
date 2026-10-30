//go:build unix

package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// signalHelper is the environment variable that makes the test binary the agent
// under test: a run that never ends, then a stop that never ends.
const signalHelper = "YC_TEST_SIGNAL_HELPER"

func TestTheFirstSignalStopsAndTheSecondExits(t *testing.T) {
	if os.Getenv(signalHelper) == "1" {
		_ = runToCompletionOrSigterm(func() error {
			fmt.Println("ready")
			time.Sleep(time.Hour)

			return nil
		})

		// As Shutdown waits for a capture whose upload hangs.
		fmt.Println("stopping")
		time.Sleep(time.Hour)

		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestTheFirstSignalStopsAndTheSecondExits$")
	cmd.Env = append(os.Environ(), signalHelper+"=1")

	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lines := make(chan string)

	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}

		close(lines)
	}()

	awaitLine := func(want string) {
		t.Helper()

		timeout := time.After(10 * time.Second)

		for {
			select {
			case line, ok := <-lines:
				require.True(t, ok, "the helper exited before printing %q", want)

				if strings.TrimSpace(line) == want {
					return
				}
			case <-timeout:
				t.Fatalf("the helper did not print %q", want)
			}
		}
	}

	awaitLine("ready")
	require.NoError(t, cmd.Process.Signal(syscall.SIGINT))
	awaitLine("stopping")

	exited := make(chan error, 1)

	go func() {
		// Drain, so the helper never blocks writing its log.
		for range lines {
		}

		exited <- cmd.Wait()
	}()

	select {
	case err := <-exited:
		t.Fatalf("the first signal ended the process instead of stopping it: %v", err)
	case <-time.After(500 * time.Millisecond):
	}

	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))

	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		require.True(t, errors.As(err, &exitErr), "want an exit status, got %v", err)
		assert.Equal(t, 1, exitErr.ExitCode())
	case <-time.After(10 * time.Second):
		t.Fatal("the second signal did not end the process; only SIGKILL would")
	}
}
