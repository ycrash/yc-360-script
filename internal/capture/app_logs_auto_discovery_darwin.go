package capture

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	"yc-agent/internal/capture/executils"
)

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	openedFiles := []string{}
	pidStr := strconv.Itoa(pid)
	output, err := executils.CommandCombinedOutput(executils.Command{"lsof", "-a", "-Fn", "-p", pidStr, "-R", "/"})

	if err != nil {
		return openedFiles, err
	}

	s := bufio.NewScanner(bytes.NewReader(output))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}

		cut, _ := strings.CutPrefix(line, "n")

		openedFiles = append(openedFiles, cut)
	}

	return openedFiles, err
}
