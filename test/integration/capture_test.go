//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapture_ThreadDump(t *testing.T) {
	t.Skip("Skipping for now")
	pid, cleanup := StartTestJVM(t)
	defer cleanup()

	ycBin := BuildYCBinary(t)
	workDir := t.TempDir()

	cmd := exec.Command(ycBin,
		"-onlyCapture",
		"-d=false", // Keep artifacts directory
		"-p", fmt.Sprintf("%d", pid),
		"-j", JavaHome,
		"-a", "test",
	)
	cmd.Dir = workDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("yc failed: %v\n%s", err, output)
	}

	// Find the created directory (yc-YYYY-MM-DDTHH-mm-ss format)
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("Failed to read work directory: %v", err)
	}

	var captureDir string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "yc-") {
			captureDir = filepath.Join(workDir, entry.Name())
			break
		}
	}

	if captureDir == "" {
		t.Fatal("Could not find capture directory")
	}

	// Verify threaddump.out contains expected content
	td, err := os.ReadFile(filepath.Join(captureDir, "threaddump.out"))
	if err != nil {
		t.Fatalf("Failed to read threaddump.out: %v", err)
	}

	content := string(td)
	if !strings.Contains(content, "Full thread dump") {
		t.Error("Thread dump missing expected header")
	}
	if !strings.Contains(content, "java.lang.Thread.State") {
		t.Error("Thread dump missing thread state info")
	}
}

func TestCapture_GCLog(t *testing.T) {
	pid, cleanup := StartTestJVM(t)
	defer cleanup()

	ycBin := BuildYCBinary(t)
	workDir := t.TempDir()

	cmd := exec.Command(ycBin,
		"-onlyCapture",
		"-d=false", // Keep artifacts directory
		"-p", fmt.Sprintf("%d", pid),
		"-j", JavaHome,
		"-a", "test",
	)
	cmd.Dir = workDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("yc failed: %v\n%s", err, output)
	}

	// Find the created directory
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("Failed to read work directory: %v", err)
	}

	var captureDir string
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "yc-") {
			captureDir = filepath.Join(workDir, entry.Name())
			break
		}
	}

	if captureDir == "" {
		t.Fatal("Could not find capture directory")
	}

	// Verify gc.log exists and has content
	gc, err := os.ReadFile(filepath.Join(captureDir, "gc.log"))
	if err != nil {
		t.Fatalf("Failed to read gc.log: %v", err)
	}

	if len(gc) == 0 {
		t.Error("GC log is empty")
	}
}
