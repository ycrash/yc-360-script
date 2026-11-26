package capture

import (
	"fmt"
	"slices"

	"yc-agent/internal/capture/dbcore"
	"yc-agent/internal/logger"
)

// anyConnected says some file's connection opened.
func anyConnected(results []dbcore.ArtifactResult) bool {
	return slices.ContainsFunc(results, func(result dbcore.ArtifactResult) bool {
		return result.Status != dbcore.StatusConnectFailed
	})
}

// logConnectFailures logs each distinct refused connection once: an error when
// none opened, a warning when some did.
func logConnectFailures(engine string, results []dbcore.ArtifactResult, describe func(error) dbcore.ConnectFailure) {
	type failure struct {
		dbcore.ConnectFailure

		detail string
		files  int
	}

	var failures []*failure

	byDescription := map[dbcore.ConnectFailure]*failure{}

	for _, result := range results {
		if result.Status != dbcore.StatusConnectFailed {
			continue
		}

		described := describe(result.ConnectErr)

		f := byDescription[described]
		if f == nil {
			f = &failure{ConnectFailure: described, detail: result.Err}
			byDescription[described] = f
			failures = append(failures, f)
		}

		f.files++
	}

	failed := 0
	for _, f := range failures {
		failed += f.files
	}

	for _, f := range failures {
		text := engine + " connection failed: " + f.Cause + "."
		if f.Fix != "" {
			text += " " + f.Fix + "."
		}

		if failed == len(results) {
			logger.Error().Msg(text + " No database data was captured. Details: " + f.detail)

			continue
		}

		logger.Warn().Msg(text + fmt.Sprintf(" %d of %d files were not captured; the rest were. Details: %s",
			f.files, len(results), f.detail))
	}
}
