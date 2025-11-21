package ondemand

import (
	"archive/zip"
	"os"
	"path/filepath"
	"testing"
)

func TestZipFolder(t *testing.T) {
	// Create a temporary directory for the test
	tempDir, err := os.MkdirTemp("", "ziptest")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Create a test folder inside temp dir
	testFolder := filepath.Join(tempDir, "testfolder")
	if err := os.Mkdir(testFolder, 0755); err != nil {
		t.Fatalf("Failed to create test folder: %v", err)
	}

	// Create test files
	testFiles := []struct {
		name    string
		content string
	}{
		{"file1.txt", "Hello, World!"},
		{"file2.txt", "Test content"},
		{"file3.log", "Log data here"},
	}

	for _, tf := range testFiles {
		filePath := filepath.Join(testFolder, tf.name)
		if err := os.WriteFile(filePath, []byte(tf.content), 0644); err != nil {
			t.Fatalf("Failed to create test file %s: %v", tf.name, err)
		}
	}

	// Change to temp dir so the zip is created there
	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Failed to get current dir: %v", err)
	}
	defer os.Chdir(originalDir)

	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Failed to change to temp dir: %v", err)
	}

	// Call ZipFolder with the relative path
	zipName, err := ZipFolder("testfolder")
	if err != nil {
		t.Fatalf("ZipFolder failed: %v", err)
	}

	// Verify the zip file was created
	zipPath := filepath.Join(tempDir, zipName)
	if _, err := os.Stat(zipPath); os.IsNotExist(err) {
		t.Fatalf("Zip file was not created at %s", zipPath)
	}

	// Open and verify the zip contents
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatalf("Failed to open zip file: %v", err)
	}
	defer reader.Close()

	if len(reader.File) != len(testFiles) {
		t.Errorf("Expected %d files in zip, got %d", len(testFiles), len(reader.File))
	}
}
