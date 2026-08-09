package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
)

// A cross-repo contract, this name and the three below: a -onlyCapture bundle
// carries no dt=, so the receiver classifies by filename alone through
// YCrashDataType.fromAgentFileName(). Each must equal that enum's agentFileName
// exactly, or the artifact is dropped with no error at either end.
const PostgresMetadataFileName = "pg_metadata.txt"

// The receiver's data types, the same contract: classification is an exact
// string match, so drift drops the artifact silently at the far end.
const pgDTMetadata = "pgMeta"

const PostgresBloatFileName = "pg_bloat.txt"

const pgDTBloat = "pgBloat"

const PostgresHealthFileName = "pg_health.txt"

const pgDTHealth = "pgHealth"

const PostgresCapacityFileName = "pg_capacity.txt"

const pgDTCapacity = "pgCapacity"

// pgSampledDataType is empty for an artifact the server team has not assigned
// one. An invented dt is dropped silently, so an unassigned artifact is written
// into the bundle and its upload skipped with a message naming the reason.
func pgSampledDataType(artifact postgres.Artifact) string {
	switch artifact.Name {
	case "pg_metadata":
		return pgDTMetadata

	case "pg_bloat":
		return pgDTBloat

	case "pg_health":
		return pgDTHealth

	case "pg_capacity":
		return pgDTCapacity
	}

	return ""
}

// PostgresCapture is the run's only database task: every artifact registers as a
// collector here rather than as a task of its own, which is what keeps one run to
// one connection. Target is a pointer so %v, %+v and %#v route through
// config.Postgres's String and GoString, which redact the password.
type PostgresCapture struct {
	Capture
	Target *config.Postgres

	// mu guards cancel, which Run sets and Kill reads from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes every artifact, and uploads each under its own dt.
// Only a file-I/O failure returns a non-nil error: a refused connection is a
// successful capture of a failure, and WrapRun overwrites Result.Msg on error -
// which would bury the connect_error the artifacts exist to record.
func (p *PostgresCapture) Run() (Result, error) {
	if p.Target == nil {
		// A result rather than an error keeps a wiring mistake out of WrapRun's
		// %#v of the task.
		return Result{Msg: "skipped postgres capture window: no postgres block configured"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.setCancel(cancel)
	defer p.setCancel(nil)

	target := postgresTarget(p.Target)
	metadata := postgres.NewMetadata(target, executils.SCRIPT_VERSION, time.Now())

	window := &postgres.Window{
		Target:   target,
		Duration: p.captureDuration(),

		// Registration order is sampling order on a shared tick, and it buys
		// something at each of the two.
		//
		// At t0, where all four sample, it is cheapest first: bloat's size
		// functions stat every relation's files, so bloat is last.
		//
		// At the closing tick, which capacity and bloat share, it is the reading
		// with no second chance first: capacity's connection and WAL blocks are
		// written once in the whole run, where bloat's second sample is one
		// endpoint of a delta whose other endpoint is already on disk.
		Collectors: []postgres.Collector{
			postgres.Health{},
			metadata,
			postgres.Capacity{},
			postgres.Bloat{},
		},
	}

	results := window.Run(ctx)

	// Into a value rather than held on the task: Metadata carries no password
	// where the collector holds the target, and WrapRun logs a failing task
	// with %#v.
	return p.uploadArtifacts(results, metadata.Collected())
}

// Kill is overridden rather than inherited: Capture.Kill returns nil when Cmd is
// nil, so the capture would ignore teardown and hold the run open.
func (p *PostgresCapture) Kill() error {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	return nil
}

func (p *PostgresCapture) setCancel(cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancel = cancel
}

// captureDuration is the configured window. Validate defaults and clamps it, so
// a nil here means a block that never went through validation.
func (p *PostgresCapture) captureDuration() time.Duration {
	if p.Target.CaptureDuration == nil {
		return config.DefaultPostgresCaptureDuration
	}

	return p.Target.CaptureDuration.Duration()
}

// uploadArtifacts keeps each capture summary in front of what its transmission
// had to say.
func (p *PostgresCapture) uploadArtifacts(artifacts []postgres.ArtifactResult, collected postgres.Metadata) (Result, error) {
	defer func() {
		for _, artifact := range artifacts {
			if artifact.File != nil {
				artifact.File.Close()
			}
		}
	}()

	var (
		messages []string
		ioErr    error
		ok       = true
	)

	for _, artifact := range artifacts {
		// The one failure the artifact cannot record about itself.
		if artifact.IOErr != nil {
			ioErr = errors.Join(ioErr, artifact.IOErr)
			continue
		}

		summary := postgresArtifactSummary(artifact, collected)

		dt := pgSampledDataType(artifact.Artifact)
		if dt == "" {
			messages = append(messages, summary+"; not uploaded: dt value not yet assigned")
			ok = false

			continue
		}

		msg, uploaded := PostData(p.Endpoint(), dt, artifact.File)
		if !uploaded {
			ok = false
		}

		messages = append(messages, summary+"; "+msg)
	}

	if ioErr != nil {
		return Result{}, fmt.Errorf("failed to capture postgres artifacts: %w", ioErr)
	}

	return Result{Msg: strings.Join(messages, " | "), Ok: ok}, nil
}

// postgresArtifactSummary gives pg_metadata.txt a reading rather than a count:
// its Once() schedule makes "1/1 samples" true and useless, where the capture
// mode decides which artifacts are possible at all. The adapter is the layer
// allowed to know which artifact is which; the window deliberately is not.
func postgresArtifactSummary(artifact postgres.ArtifactResult, collected postgres.Metadata) string {
	if artifact.Artifact.Name != "pg_metadata" {
		return postgresArtifactMessage(artifact)
	}

	// The collector never reached the server, so the window holds the only
	// account of why.
	if artifact.Status == postgres.StatusConnectFailed {
		collected.ConnectError = artifact.Err
	}

	return postgresResultMessage(collected)
}

// postgresArtifactMessage interpolates only values the window already redacted.
func postgresArtifactMessage(artifact postgres.ArtifactResult) string {
	summary := fmt.Sprintf("%s written (%d/%d samples)",
		artifact.Artifact.FileName, artifact.SamplesWritten, artifact.SamplesExpected)

	switch artifact.Status {
	case postgres.StatusConnectFailed:
		return summary + "; postgres connect failed: " + artifact.Err

	case postgres.StatusCancelled:
		return summary + "; window cancelled"

	case postgres.StatusDeadlineExceeded:
		return summary + "; window deadline exceeded"

	case postgres.StatusPartial:
		return summary + "; last sample error: " + artifact.Err
	}

	return summary
}

// postgresTarget keeps the capture package free of a config import.
func postgresTarget(pg *config.Postgres) postgres.Target {
	return postgres.Target{
		Host:     pg.Host,
		Port:     pg.Port,
		Database: pg.Database,
		Username: pg.Username,
		Password: pg.Password,
		SSLMode:  pg.SSLMode,
	}
}

// postgresResultMessage interpolates only values already stripped of the password.
func postgresResultMessage(metadata postgres.Metadata) string {
	if metadata.ConnectError != "" {
		return fmt.Sprintf("%s written; postgres connect failed: %s",
			PostgresMetadataFileName, metadata.ConnectError)
	}

	parts := []string{fmt.Sprintf("%s written (mode=%s)", PostgresMetadataFileName, metadata.CaptureMode)}

	// Named neutrally rather than as denials: a statement timeout reaches here
	// identically, and calling that a denial misdirects the reader.
	for _, probe := range []struct{ name, err string }{
		{"server facts query failed", metadata.QueryError},
		{"pg_current_logfile failed", metadata.CurrentLogfileError},
		{"pg_stat_replication probe failed", metadata.ReplicationProbeError},
	} {
		if probe.err != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", probe.name, probe.err))
		}
	}

	return strings.Join(parts, "; ")
}
