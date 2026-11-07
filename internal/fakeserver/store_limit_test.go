//go:build linux || darwin

package fakeserver_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"yc-agent/internal/fakeserver"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sizeLimitDirEnv = "FAKESERVER_SIZE_LIMIT_DIR"
	sizeLimit       = 16 << 10
	limitedBody     = 84_000
)

// A body the disk takes only part of is not kept, so Body never returns the part
// as the upload. A child process runs the fake under a file-size limit.
func TestABodyCutShortOnDiskIsNotKept(t *testing.T) {
	if dir := os.Getenv(sizeLimitDirEnv); dir != "" {
		uploadUnderASizeLimit(t, dir)

		return
	}

	dir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=^TestABodyCutShortOnDiskIsNotKept$", "-test.count=1")
	cmd.Env = append(os.Environ(), sizeLimitDirEnv+"="+dir)

	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)

	jsonl, err := os.ReadFile(filepath.Join(dir, "requests.jsonl"))
	require.NoError(t, err)

	var r fakeserver.Request
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(jsonl), &r))

	assert.Equal(t, http.StatusOK, r.Status, "the agent is not at fault")
	assert.Equal(t, int64(limitedBody), r.Bytes, "the whole body arrived")
	assert.Empty(t, r.File, "no file claims to hold it")
	assert.Equal(t, []string{"requests.jsonl", run + "/requests.txt"}, files(t, dir), "and the part on disk is gone")
}

func uploadUnderASizeLimit(t *testing.T, dir string) {
	var limit syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_FSIZE, &limit))

	limit.Cur = sizeLimit
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_FSIZE, &limit))

	srv := fakeserver.Start(t, fakeserver.Options{Dir: dir})

	status, _ := post(t, srv, srv.URLFor(fakeserver.Receiver)+"&dt=td", strings.Repeat("x", limitedBody))
	require.Equal(t, http.StatusOK, status)
}
