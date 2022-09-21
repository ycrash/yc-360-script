package util

import (
	"os"
	"testing"
)

func TestGetTailOfFile(t *testing.T) {

	fileLogPath := "testdata/gc.log"
	fileLog, _ := os.Open(fileLogPath)

	//t.Error("test error")
	fileOut, fileName, err := GetTailOfFile(fileLog, fileLogPath, 4)

	if err != nil {
		t.Errorf("Error occur: %s", err.Error())
	}

	if fileName == fileLogPath {
		t.Errorf("result file %s is not as expected %s", fileName, fileLogPath)
	}

	buf, _ := os.ReadFile(fileName)
	content := string(buf)
	expected := "line 7\nline 8\nline 9\nline 10\n"
	if content != expected {
		t.Errorf("content \n%s\n is not as expected \n%s\n", content, expected)
	}

	_ = fileOut.Close()
}
