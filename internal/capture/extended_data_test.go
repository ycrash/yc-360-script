package capture

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// setupMockServer creates a mock HTTP server for testing
func setupMockServer(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}))
	t.Cleanup(server.Close)
	return server
}

// createTestScript creates a cross-platform test script
func createTestScript(t *testing.T, outputFile string) string {
	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(scriptDir) })

	var scriptContent string
	var scriptFile string

	if runtime.GOOS == "windows" {
		scriptContent = fmt.Sprintf("@echo off\r\necho test-data > \"%s\"\r\n", outputFile)
		scriptFile = filepath.Join(scriptDir, "test-script.bat")
	} else {
		scriptContent = fmt.Sprintf("#!/bin/bash\necho test-data > \"%s\"\n", outputFile)
		scriptFile = filepath.Join(scriptDir, "test-script.sh")
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	return absScriptFile
}

// createLongRunningScript creates a script that runs for a specified duration
func createLongRunningScript(t *testing.T, duration time.Duration) (string, string) {
	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(scriptDir) })

	var scriptContent string
	var scriptFile string

	if runtime.GOOS == "windows" {
		scriptContent = fmt.Sprintf("@echo off\r\ntimeout /t %d /nobreak\r\n", int(duration.Seconds()))
		scriptFile = filepath.Join(scriptDir, "test-script.bat")
	} else {
		scriptContent = fmt.Sprintf("#!/bin/bash\nsleep %d\n", int(duration.Seconds()))
		scriptFile = filepath.Join(scriptDir, "test-script.sh")
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	return absScriptFile, scriptDir
}

// pass a valid ed script and ed data folder
// then upload file and returns result OK
func TestExtendedData_Run_Success(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")

	// Step 3: Create test script
	absScriptFile := createTestScript(t, outputFile)

	// Step 4: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up mock http server
	server := setupMockServer(t)
	ed.SetEndpoint(server.URL + "/ycrash-receiver?de=localhost")

	// Step 5: Run the script
	result, err := ed.Run()
	if err != nil {
		logPath := filepath.Join(testDir, "script_execution.log")
		if logContent, readErr := os.ReadFile(logPath); readErr == nil {
			t.Logf("📄 script_execution.log:\n%s", string(logContent))
		}
		t.Fatalf("Run failed: %v", err)
	}
	if !result.Ok {
		t.Fatalf("Run not OK: %s", result.Msg)
	}

	// Step 6: Check if output file was created
	if _, err := os.Stat(outputFile); err != nil {
		t.Errorf("Expected output file not created: %v", err)
	}
}

// sets a timeout and the test case checks whether the
// timeout is happening
func TestExtendedData_Run_ScriptTimeout(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Create a long-running script (5 seconds)
	absScriptFile, _ := createLongRunningScript(t, 5*time.Second)

	// Step 3: Create the ExtendedData struct with very short timeout
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    100 * time.Millisecond, // Very short timeout
	}

	// Step 4: Run the script and expect timeout
	_, err = ed.Run()
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Errorf("expected timeout error, got: %v", err)
	}
}

// the following test checks whether already existing file is not
// deleted after the extended data is run
func TestExtendedData_Run_FolderDeleting(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 1.1: Create hello.txt inside testDir
	helloFilePath := filepath.Join(testDir, "hello.txt")
	err = os.WriteFile(helloFilePath, []byte("Hello, world!"), 0666)
	if err != nil {
		t.Fatalf("Failed to create hello.txt: %v", err)
	}

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")

	// Step 3: Create test script
	absScriptFile := createTestScript(t, outputFile)

	// Step 4: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up mock http server
	server := setupMockServer(t)
	ed.SetEndpoint(server.URL + "/ycrash-receiver?de=localhost")

	// Step 5: Run the script
	ed.Run()

	// Step 6: Assert hello.txt still exists
	if _, err := os.Stat(helloFilePath); os.IsNotExist(err) {
		t.Errorf("hello.txt was expected to exist, but it does not")
	}
}

// given the script folder same as extended data folder
func TestExtendedData_Run_ScriptFolderSameAsDataFolder(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")

	// Step 3: Create test script
	absScriptFile := createTestScript(t, outputFile)

	// Step 4: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up mock http server
	server := setupMockServer(t)
	ed.SetEndpoint(server.URL + "/ycrash-receiver?de=localhost")

	// Step 5: Run the script
	result, _ := ed.Run()
	if !result.Ok {
		t.Fatalf("Run not OK: %s", result.Msg)
	}
}

// pass empty data folder and valid ed script
func TestExtendedData_Run_EmptyDataFolder(t *testing.T) {
	testDir := "" // given empty folder

	// Step 1: Create test script
	tempOutputFile := filepath.Join(os.TempDir(), "output.log")
	absScriptFile := createTestScript(t, tempOutputFile)

	// Step 2: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// Step 3: Run the script
	result, err := ed.Run()
	fmt.Printf("result msg->%s", result.Msg)

	if result.Ok {
		t.Fatalf("Run should not be OK: %s", result.Msg)
	}
	if !strings.Contains(result.Msg, "ExtendedData: failed to create data folder") {
		t.Errorf("expected data folder creation error, got: %v", err)
	}
}

// given empty script and valid data folder
func TestExtendedData_Run_EmptyScript(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Create the ExtendedData struct with empty script
	ed := &ExtendedData{
		Script:     "",
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// Step 3: Run the script
	result, err := ed.Run()
	if result.Ok {
		t.Fatalf("Run should not be OK: %s", result.Msg)
	}
	if !strings.Contains(result.Msg, "ExtendedData: error while executing custom script:") {
		t.Errorf("expected custom script error, got: %v", err)
	}
}

// given data folder as relative path
// should generate the output file
func TestExtendedData_Run_RelativePath(t *testing.T) {
	testDir := "." // given relative path

	// Step 1: Define paths
	outputFile := filepath.Join(testDir, "output.log")

	// Step 2: Create test script
	absScriptFile := createTestScript(t, outputFile)

	// Step 3: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up mock http server
	server := setupMockServer(t)
	ed.SetEndpoint(server.URL + "/ycrash-receiver?de=localhost")

	// Step 4: Run the script
	result, err := ed.Run()
	if err != nil {
		logPath := filepath.Join(testDir, "script_execution.log")
		if logContent, readErr := os.ReadFile(logPath); readErr == nil {
			t.Logf("📄 script_execution.log:\n%s", string(logContent))
		}
		t.Fatalf("Run failed: %v", err)
	}
	if !result.Ok {
		t.Fatalf("Run not OK: %s", result.Msg)
	}

	// Step 5: Check if output file was created
	if _, err := os.Stat(outputFile); err != nil {
		t.Errorf("Expected output file not created: %v", err)
	}

	// Clean up the output file created in current directory
	os.Remove(outputFile)
	os.Remove(filepath.Join(testDir, "script_execution.log"))

	// Clean up any ed-* files created in current directory
	entries, _ := os.ReadDir(".")
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "ed-") {
			os.Remove(entry.Name())
		}
	}
}
