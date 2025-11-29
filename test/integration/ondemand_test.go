//go:build integration

package integration

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOnDemand_CaptureOnly(t *testing.T) {
	pid, cleanup := StartTestJVM(t)
	defer cleanup()

	ycBin := BuildYCBinary(t)
	workDir := t.TempDir()

	// Execute yc in capture-only mode
	cmd := exec.Command(ycBin,
		"-onlyCapture",
		"-p", fmt.Sprintf("%d", pid),
		"-j", JavaHome,
		"-a", "test-app",
	)
	cmd.Dir = workDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("yc failed: %v\n%s", err, output)
	}

	// Verify ZIP file created
	zipFiles, err := filepath.Glob(filepath.Join(workDir, "yc-*.zip"))
	if err != nil {
		t.Fatalf("Failed to glob zip files: %v", err)
	}
	if len(zipFiles) != 1 {
		t.Fatalf("Expected 1 zip file, found %d", len(zipFiles))
	}

	// Validate ZIP contents
	zipFile := zipFiles[0]
	ValidateZipContents(t, zipFile, []string{
		"top.out",
		"vmstat.out",
		"ps.out",
		"netstat.out",
		"gc.log",
		"threaddump.out",
	})
}

func TestOnDemand_FullUpload(t *testing.T) {
	t.Skip("Skipping full upload test for now")

	pid, cleanup := StartTestJVM(t)
	defer cleanup()

	ycBin := BuildYCBinary(t)

	// Track received uploads in mock server
	mockServerClient := &http.Client{Timeout: 10 * time.Second}

	// Execute yc with upload
	cmd := exec.Command(ycBin,
		"-p", fmt.Sprintf("%d", pid),
		"-j", JavaHome,
		"-a", "test-app",
		"-s", MockServerURL,
		"-k", "test-api-key-123",
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("yc failed: %v\n%s", err, output)
	}

	// Verify uploads received by mock server
	resp, err := mockServerClient.Get(MockServerURL + "/verify-uploads?app=test-app")
	if err != nil {
		t.Fatalf("Failed to verify uploads: %v", err)
	}
	defer resp.Body.Close()

	var uploads map[string]bool
	json.NewDecoder(resp.Body).Decode(&uploads)

	expectedUploads := []string{"top", "vmstat", "ps", "ns", "gc", "td"}
	for _, dt := range expectedUploads {
		if !uploads[dt] {
			t.Errorf("Missing upload for data type: %s", dt)
		}
	}
}

func ValidateZipContents(t *testing.T, zipPath string, expectedFiles []string) {
	t.Helper()

	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("Failed to open zip: %v", err)
	}
	defer r.Close()

	found := make(map[string]bool)
	for _, f := range r.File {
		// Strip directory prefix if present
		name := filepath.Base(f.Name)
		found[name] = true
	}

	for _, expected := range expectedFiles {
		if !found[expected] {
			// List what we actually found for debugging
			var foundFiles []string
			for name := range found {
				foundFiles = append(foundFiles, name)
			}
			t.Errorf("Missing expected file in zip: %s\nFound files: %v", expected, strings.Join(foundFiles, ", "))
		}
	}
}
