//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	BuggyAppHost    = "buggyapp"
	MockServerURL   = "http://mock-server:8080"
	defaultJavaHome = "/usr/lib/jvm/java-11-openjdk"
	TestTimeout     = 120 * time.Second
)

var (
	// JavaHome is the path to Java installation, configurable via JAVA_HOME environment variable
	JavaHome = getJavaHome()
)

// getJavaHome returns the Java home directory from environment or default
func getJavaHome() string {
	if javaHome := os.Getenv("JAVA_HOME"); javaHome != "" {
		return javaHome
	}
	return defaultJavaHome
}

// StartTestJVM launches a Java process for testing
// Note: This function expects to run inside the Docker test environment
// where it can communicate with the buggyapp container via the shared network
func StartTestJVM(t *testing.T) (pid int, cleanup func()) {
	t.Helper()

	// When running in the test runner container, we need to get the PID
	// from within the buggyapp container. We use 'docker exec' for this.
	// Note: This requires Docker socket access OR we could use HTTP-based
	// process discovery if buggyapp exposed a /pid endpoint.
	cmd := exec.Command("docker", "exec", "yc-test-buggyapp",
		"sh", "-c", "pgrep -f 'java.*buggyapp'")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Failed to get Java PID: %v", err)
	}

	_, err = fmt.Sscanf(string(out), "%d", &pid)
	if err != nil {
		t.Fatalf("Failed to parse PID: %v", err)
	}

	// Verify the process is still running
	cleanup = func() {
		// Optionally verify process is still running after test
		cmd := exec.Command("docker", "exec", "yc-test-buggyapp",
			"sh", "-c", fmt.Sprintf("kill -0 %d 2>/dev/null", pid))
		if err := cmd.Run(); err != nil {
			t.Logf("Warning: Java process %d is no longer running", pid)
		}
	}

	return pid, cleanup
}

// BuildYCBinary compiles the yc binary for testing
func BuildYCBinary(t *testing.T) string {
	t.Helper()

	binPath := filepath.Join(t.TempDir(), "yc")
	cmd := exec.Command("go", "build",
		"-o", binPath,
		"-ldflags", "-s -w",
		"/workspace/cmd/yc")

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Build failed: %v\n%s", err, out)
	}

	// Verify binary was created and is executable
	info, err := os.Stat(binPath)
	if err != nil {
		t.Fatalf("Binary not created at %s: %v", binPath, err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("Binary at %s is not executable (mode: %v)", binPath, info.Mode())
	}

	return binPath
}

// WaitForFile polls for file existence
func WaitForFile(path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for %s", path)
		case <-ticker.C:
			if _, err := os.Stat(path); err == nil {
				return nil
			}
		}
	}
}
