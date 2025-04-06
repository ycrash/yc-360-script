package capture

import (
	"bufio"
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"yc-agent/internal/capture/executils"
)

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	openedFiles := []string{}
	pidStr := strconv.Itoa(pid)
	output, err := executils.CommandCombinedOutput(executils.Command{"lsof", "-Fn", "-p", pidStr})

	if err != nil {
		return openedFiles, err
	}

	s := bufio.NewScanner(bytes.NewReader(output))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}

		path := strings.TrimPrefix(line, "n")
		if path == "" {
			continue
		}

		openedFiles = append(openedFiles, path)
	}

	if err := s.Err(); err != nil {
		return openedFiles, fmt.Errorf("scanner error reading lsof output: %w", err)
	}

	return openedFiles, nil
}
