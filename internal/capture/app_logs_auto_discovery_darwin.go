package capture

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"

	"yc-agent/internal/capture/executils"
)

func GetOpenedFilesByProcess(pid int) ([]string, error) {
	pidStr := strconv.Itoa(pid)
	output, err := executils.CommandCombinedOutput(executils.Command{"lsof", "-a", "-Fn", "-p", pidStr, "-R", "/"})

	openedFiles := []string{}
	if err != nil {
		return openedFiles, err
	}

	s := bufio.NewScanner(bytes.NewReader(output))
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "n") {
			continue
		}

		cut, _ := cutPrefix(line, "n")

		openedFiles = append(openedFiles, cut)
	}

	return openedFiles, err
}

// CutPrefix returns s without the provided leading prefix string
// and reports whether it found the prefix.
// If s doesn't start with prefix, CutPrefix returns s, false.
// If prefix is the empty string, CutPrefix returns s, true.
// This is a shim for strings.CutPrefix. Once we upgraded go version to a recent one,
// this should be replaced with strings.CutPrefix.
func cutPrefix(s, prefix string) (after string, found bool) {
	if !strings.HasPrefix(s, prefix) {
		return s, false
	}
	return s[len(prefix):], true
}
