//go:build linux

package capture

import (
	"bufio"
	"bytes"
	"strconv"
	"strings"
	"yc-agent/internal/capture/executils"

	"github.com/mitchellh/go-ps"
)

// GetDockerID retrieves the Docker container ID associated with a given process ID.
// It works by traversing up the process tree and matching PIDs with Docker's process list.
// Returns an empty string if no container ID is found.
func GetDockerID(pid int) (string, error) {
	// Get all parent PIDs in the process tree
	pids, err := getPIDChain(pid)
	if err != nil {
		return "", err
	}

	// Get Docker process information
	output, err := executils.CommandCombinedOutput(executils.DockerInfo)
	if err != nil {
		return "", err
	}

	// Scan through Docker output looking for matching PIDs
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		for _, pid := range pids {
			prefix := strconv.Itoa(pid) + " "
			if strings.HasPrefix(line, prefix) {
				// Return container ID by trimming PID prefix from the line
				return line[len(prefix):], nil
			}
		}
	}

	// No matching container ID found
	return "", nil
}

// getPIDChain returns a slice of process IDs representing the process tree
// starting from the given PID up to the root process (PID 1).
// The returned slice includes the input PID and all its parent PIDs.
func getPIDChain(pid int) ([]int, error) {
	var pids []int

	for {
		process, err := ps.FindProcess(pid)

		// Process not found - stop here
		if process == nil {
			return pids, nil
		}

		if err != nil {
			return nil, err
		}

		pids = append(pids, pid)
		pid = process.PPid()
	}
}

// DockerCopy copies files between the host and a Docker container.
// dst and src should be in the format "container:path" or just "path" for host files.
func DockerCopy(dst, src string) error {
	return executils.CommandRun(executils.Append(executils.DockerCP, src, dst))
}

// DockerExecute runs a command inside a Docker container and returns its output.
// The first argument should be the container ID, followed by the command and its arguments.
func DockerExecute(args ...string) ([]byte, error) {
	return executils.CommandCombinedOutput(executils.Append(executils.DockerExec, args...))
}
