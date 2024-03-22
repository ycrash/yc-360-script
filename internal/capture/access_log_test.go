package capture

import (
	"fmt"
	"os"
	"testing"
)

func TestPrepareLogsForSendingFull(t *testing.T) {
	srcPath := "access.log"
	dstPath := srcPath + ".full"
	initPosition := int64(0)
	file, pos, err := prepareLogsForSending(srcPath, initPosition, dstPath)

	if err != nil {
		t.Error(err)
	}
	fInfo, _ := os.Stat(srcPath)
	expectedPos := fInfo.Size()
	if pos != expectedPos {
		t.Errorf("current position %d is not as expected %d", pos, expectedPos)
	}

	if file == nil {
		t.Errorf("destination file is empty")
	}
	_ = file.Close()
}

func TestPrepareLogsForSendingRest(t *testing.T) {
	srcPath := "access.log"
	dstPath := srcPath + ".rest"
	initPosition := int64(6)
	file, pos, err := prepareLogsForSending(srcPath, initPosition, dstPath)

	if err != nil {
		t.Error(err)
	}
	fInfo, _ := os.Stat(srcPath)
	expectedPos := fInfo.Size()
	if pos != expectedPos {
		t.Errorf("current position %d is not as expected %d", pos, expectedPos)
	}

	if file == nil {
		t.Errorf("destination file is empty")
	}
}

func TestPrepareLogsForSending(t *testing.T) {
	// emulating sequence of operations
	initPosition := int64(0)
	srcPaths := []string{"access.1.log", "access.2.log", "access.3.log"}
	for q, srcPath := range srcPaths {
		dstPath := fmt.Sprintf("%s.%d.chunk", srcPath, q+1)

		file, pos, err := prepareLogsForSending(srcPath, initPosition, dstPath)
		initPosition = pos
		if err != nil {
			t.Error(err)
		}
		_ = file.Close()
		if file == nil {
			t.Errorf("destination file is empty")
		}
		fInfo, err := os.Stat(srcPath)
		if initPosition != fInfo.Size() {
			t.Errorf("current position %d is not as expected %d", initPosition, fInfo.Size())
		}
	}
}
