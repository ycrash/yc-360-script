package capture

import (
	"errors"
	"fmt"
	"io/fs"
)

// Labels for logPathProblem, as the user sees them at the start of a sentence.
const (
	appLogLabel    = "Application log"
	accessLogLabel = "Access log"
)

// logPathProblem explains, in one sentence, why a configured application or
// access log path can't be captured. err is the error from looking the path
// up (zglob.Glob or os.Stat); it returns nil when err is nil.
func logPathProblem(label, path string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("%s %s doesn't exist, so it isn't captured. Check the path and that the file exists", label, path)
	case errors.Is(err, fs.ErrPermission):
		return fmt.Errorf("%s %s can't be read because access is denied, so it isn't captured. Run yc as a user that can read it", label, path)
	default:
		return fmt.Errorf("%s %s can't be read, so it isn't captured: %w", label, path, err)
	}
}
