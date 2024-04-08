//go:build linux
// +build linux

package capture

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"yc-agent/internal/capture/executils"

	"github.com/mitchellh/go-ps"
)

func GetDockerID(pid int) (id string, err error) {
	pids, err := getPIDChain(pid)
	if err != nil {
		return
	}
	output, err := executils.CommandCombinedOutput(executils.DockerInfo)
	if err != nil {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		for _, pid := range pids {
			prefix := strconv.Itoa(pid) + " "
			if strings.HasPrefix(line, prefix) {
				id = line[len(prefix):]
				return
			}
		}
	}
	return
}

func getPIDChain(pid int) (pids []int, err error) {
	var process ps.Process
	for {
		process, err = ps.FindProcess(pid)
		if process == nil {
			return
		}
		if err != nil {
			return
		}
		pids = append(pids, pid)
		pid = process.PPid()
	}
}

func DockerCopy(dst string, src string) (err error) {
	err = executils.CommandRun(executils.Append(executils.DockerCP, src, dst))
	return
}

func DockerExecute(args ...string) (output []byte, err error) {
	output, err = executils.CommandCombinedOutput(executils.Append(executils.DockerExec, args...))
	return
}
