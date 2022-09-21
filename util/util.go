package util

import (
	"bufio"
	"io"
	"os"
)

func GetTailOfFile(gc *os.File, gcFile string, linesTailCount int) (*os.File, string, error) {
	// set file pos to start
	if _, err := gc.Seek(0, io.SeekStart); err != nil {
		return gc, gcFile, err
	}

	// count lines
	fileScanner := bufio.NewScanner(gc)
	fileScanner.Split(bufio.ScanLines)
	linesCount := 0
	for fileScanner.Scan() {
		linesCount++
	}

	// if count lines less when limit
	if linesCount <= linesTailCount {
		return gc, gcFile, nil
	}

	// set file pos to start
	if _, err := gc.Seek(0, io.SeekStart); err != nil {
		return gc, gcFile, err
	}

	// reopen scanner and skip lines from start of file
	fileScanner = bufio.NewScanner(gc)
	fileScanner.Split(bufio.ScanLines)
	for linesCount > linesTailCount {
		fileScanner.Scan()
		linesCount--
	}

	// store rest of lines into separate file
	// create file with a tailed information
	tailFileName := gcFile + ".tail"
	tailFile, err := os.Create(tailFileName)
	if err != nil {
		return gc, gcFile, err
	}

	// write rest of lines into tailed file
	fileWriter := bufio.NewWriter(tailFile)

	for fileScanner.Scan() {
		if _, err := fileWriter.WriteString(fileScanner.Text() + "\n"); err != nil {
			_ = tailFile.Close()
			return gc, gcFile, err
		}
	}
	_ = fileWriter.Flush()
	_ = gc.Close()

	return tailFile, tailFileName, nil
}
