package capture

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"yc-agent/internal/config"

	"github.com/mattn/go-zglob"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLogPathProblem(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(existing, []byte("line\n"), 0o600))

	cases := []struct {
		name, path, want string
	}{
		{"existing file", existing, ""},
		{"pattern with a match", filepath.Join(dir, "*.log"), ""},
		// A pattern that matches nothing is skipped without a message, as before.
		{"pattern with no match", filepath.Join(dir, "*.txt"), ""},
		{"missing file", filepath.Join(dir, "yc1.log"), "Application log " + filepath.Join(dir, "yc1.log") + " doesn't exist, so it isn't captured."},
		{"missing folder", filepath.Join(dir, "nodir", "app.log"), "doesn't exist, so it isn't captured."},
	}
	for _, c := range cases {
		// Real zglob results, so the test follows how it reports each case.
		_, err := zglob.Glob(c.path)
		problem := logPathProblem(appLogLabel, c.path, err)
		if c.want == "" {
			assert.NoError(t, problem, c.name)
			continue
		}
		require.Error(t, problem, c.name)
		assert.Contains(t, problem.Error(), c.want, c.name)
		assert.NotContains(t, problem.Error(), "invalid glob pattern", c.name)
	}

	// Errors other than "doesn't exist" keep their cause.
	denied := logPathProblem(accessLogLabel, "/var/log/access.log", fs.ErrPermission)
	assert.Contains(t, denied.Error(), "Access log /var/log/access.log can't be read because access is denied")
	other := logPathProblem(appLogLabel, "/x.log", errors.New("disk error"))
	assert.Contains(t, other.Error(), "can't be read, so it isn't captured: disk error")
}

func TestExpandPathsReportsEachPath(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(good, []byte("line\n"), 0o600))
	missing := filepath.Join(dir, "yc1.log")

	files, problems := expandPaths([]string{good, missing, filepath.Join(dir, "*.txt")})

	assert.Equal(t, []string{good}, files, "the existing file is still captured")
	require.Len(t, problems, 1, "a pattern that matches nothing gives no message")
	assert.Contains(t, problems[0].Error(), "Application log "+missing+" doesn't exist")
}

func TestAppLogM3ReportsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "yc1.log")
	appLog := NewAppLogM3()
	appLog.Paths = map[int]config.AppLogs{1000: {config.AppLog(missing)}}

	result, err := appLog.Run()

	// Reported in the result only: an error would make WrapRun log a dump of
	// the whole task every M3 cycle.
	require.NoError(t, err)
	assert.False(t, result.Ok)
	assert.Contains(t, result.Msg, "Application log "+missing+" doesn't exist")
}

func TestAccessLogM3ReportsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "access-%t.log")
	accessLog := NewAccessLogM3()
	accessLog.SetPaths(map[int][]AccessLogEntry{1000: {{Path: config.AccessLogPath(missing), Format: "%h %t %r %s"}}})

	result, err := accessLog.Run()

	require.NoError(t, err)
	assert.False(t, result.Ok)
	assert.Contains(t, result.Msg, "Access log ")
	assert.Contains(t, result.Msg, "doesn't exist")
	assert.NotContains(t, result.Msg, "%t", "the date is filled in, so the user sees the real file name")
}
