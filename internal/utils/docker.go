//go:build !linux
// +build !linux

package utils

func GetDockerID(pid int) (id string, err error) {
	return
}

func DockerCopy(dst string, src string) (err error) {
	return
}

func DockerExecute(args ...string) (output []byte, err error) {
	return
}
