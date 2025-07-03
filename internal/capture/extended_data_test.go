package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// pass a valid ed script and ed data folder
// then upload file and returns result OK
// NOTE: It is important to start the http server on port 8080
// before running the test cases
func TestExtendedData_Run_Success(t *testing.T) {
	// Step 1: Create a temporary test directory
	testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 3: Create a simple .bat script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh \r\necho test-data > \"" + outputFile + "\"\r\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 4: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 5: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 6: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}
	// set up http endpoint
	ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// Step 7: Run the script
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

	// Step 8: Check if output file was created
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

	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 3: Create a simple .bat script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh \r\necho test-data > \"" + outputFile + "\"\r\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 4: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 5: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 6: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    1 * time.Millisecond,
	}
	// set up http endpoint

	// Step 7: Run the script
	_, err = ed.Run()
	if err == nil || !strings.Contains(err.Error(), "custom script execution timed out") {
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

	// Step 2: Create another temporary directory for the script
	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(scriptDir)

	// Step 3: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 4: Create a simple .bat or shell script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh\necho test-data > \"" + outputFile + "\"\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 5: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 6: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 7: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up http endpoint
	ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// // Step 8: Run the script
	_, err = ed.Run()

	// Step 9: Assert hello.txt still exists
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

	// Step 2: Create another temporary directory for the script
	scriptDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(scriptDir)

	// Step 3: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 4: Create a simple .bat or shell script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh\necho test-data > \"" + outputFile + "\"\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 5: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 6: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 7: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up http endpoint
	ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// // Step 8: Run the script
	result, err := ed.Run()
	if !result.Ok {
		t.Fatalf("Run not OK: %s", result.Msg)
	}
}

// pass empty data folder and valid ed script
func TestExtendedData_Run_EmptyDataFolder(t *testing.T) {
	// Step 1: Create a temporary test directory
	// testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	// if err != nil {
	// 	t.Fatalf("Failed to create temp dir: %v", err)
	// }
	// defer os.RemoveAll(testDir)

	testDir := "" // given empty folder

	// Step 2: Create another temporary directory for the script
	scriptDir, err := os.MkdirTemp("", "extended-batch-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(scriptDir)

	// Step 3: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 4: Create a simple .bat or shell script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh\necho test-data > \"" + outputFile + "\"\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 5: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 6: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 7: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up http endpoint
	//ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// // Step 8: Run the script
	result, err := ed.Run()
	fmt.Printf("result msg->%s", result.Msg)

	if result.Ok {
		t.Fatalf("Run not OK: %s", result.Msg)
	}
	if !strings.Contains(result.Msg, "ExtendedData: failed to create data folder") {
		t.Errorf("extended data folder creation, got: %v", err)
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

	// Step 2: Create another temporary directory for the script
	// scriptDir, err := os.MkdirTemp("", "extended-batch-test-*")
	// if err != nil {
	// 	t.Fatalf("Failed to create temp dir: %v", err)
	// }
	// defer os.RemoveAll(scriptDir)

	scriptDir := ""

	// Step 3: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 4: Create a simple .bat or shell script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh\necho test-data > \"" + outputFile + "\"\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 5: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 6: Absolute path
	// absScriptFile, err := filepath.Abs(scriptFile)
	// if err != nil {
	// 	t.Fatalf("Failed to get absolute path of script: %v", err)
	// }

	// Step 7: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     "",
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}

	// set up http endpoint
	//	ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// // Step 8: Run the script
	result, err := ed.Run()
	if result.Ok {
		t.Fatalf("Run  OK: %s", result.Msg)
	}
	if !strings.Contains(result.Msg, "ExtendedData: error while executing custom script:") {
		t.Errorf("extended data custom script, got: %v", err)
	}
}

// given data folder as relative path
// should generate the output file
func TestExtendedData_Run_RelativePath(t *testing.T) {
	// Step 1: Create a temporary test directory
	// testDir, err := os.MkdirTemp("", "extended-batch-test-*")
	// if err != nil {
	// 	t.Fatalf("Failed to create temp dir: %v", err)
	// }
	// defer os.RemoveAll(testDir)

	testDir := "." //given relative path

	scriptDir, err := os.MkdirTemp("", "extended-script-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	// Step 2: Define paths
	outputFile := filepath.Join(testDir, "output.log")
	scriptFile := filepath.Join(scriptDir, "test-script.bat")

	// Step 3: Create a simple .bat script
	var scriptContent = ""
	if runtime.GOOS == "windows" {
		scriptContent = "@echo off\r\necho test-data > \"" + outputFile + "\"\r\n"
	} else {
		scriptContent = "#!/bin/sh \r\necho test-data > \"" + outputFile + "\"\r\n"
	}

	err = os.WriteFile(scriptFile, []byte(scriptContent), 0777)
	if err != nil {
		t.Fatalf("Failed to write script: %v", err)
	}

	// Step 4: Confirm script file exists
	if _, err := os.Stat(scriptFile); os.IsNotExist(err) {
		t.Fatalf("Script file does not exist: %s", scriptFile)
	}

	// Step 5: Absolute path
	absScriptFile, err := filepath.Abs(scriptFile)
	if err != nil {
		t.Fatalf("Failed to get absolute path of script: %v", err)
	}

	// Step 6: Create the ExtendedData struct
	ed := &ExtendedData{
		Script:     absScriptFile,
		DataFolder: testDir,
		Timeout:    3 * time.Second,
	}
	// set up http endpoint
	ed.SetEndpoint("http://localhost:8080/ycrash-receiver?de=localhost")

	// Step 7: Run the script
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

	// Step 8: Check if output file was created
	if _, err := os.Stat(outputFile); err != nil {
		t.Errorf("Expected output file not created: %v", err)
	}
}
