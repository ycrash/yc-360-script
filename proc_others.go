//go:build !windows
// +build !windows

package shell

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
	"strings"
)

func GetTopCpu() (pid int, err error) {
	output, err := CommandCombinedOutput(ProcessTopCPU)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		p := strings.Index(line, "java")
		if p >= 0 {
			columns := strings.Split(line, " ")
			var col []string
			for _, column := range columns {
				if len(column) > 0 {
					col = append(col, column)
					if len(col) > 0 {
						break
					}
				}
			}
			if len(col) > 0 {
				id := strings.TrimSpace(col[0])
				pid, err = strconv.Atoi(id)
				if err != nil {
					continue Next
				}
				if pid == cpid {
					continue Next
				}
				return
			}
		}
	}
	return
}

func GetTopMem() (pid int, err error) {
	output, err := CommandCombinedOutput(ProcessTopMEM)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	cpid := os.Getpid()
Next:
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		p := strings.Index(line, "java")
		if p >= 0 {
			columns := strings.Split(line, " ")
			var col []string
			for _, column := range columns {
				if len(column) > 0 {
					col = append(col, column)
					if len(col) > 0 {
						break
					}
				}
			}
			if len(col) > 0 {
				id := strings.TrimSpace(col[0])
				pid, err = strconv.Atoi(id)
				if err != nil {
					continue Next
				}
				if pid == cpid {
					continue Next
				}
				return
			}
		}
	}
	return
}
