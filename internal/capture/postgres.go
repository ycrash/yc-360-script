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

// PostgresMetadataFileName restates postgres.MetadataCollector's own
// Artifact().FileName so callers outside that package have a constant to name;
// TestPostgresMetadataFileNameMatchesTheArtifact keeps the two from drifting.
// Database artifacts keep engine-specific prefixes: pg_ here, my_ and ora_ to
// follow.
//
// This name and the two below are a cross-repo contract: a -onlyCapture bundle
// carries no dt=, so the receiver classifies these files by filename alone
// through YCrashDataType.fromAgentFileName(). Each must equal that enum's
// agentFileName exactly, or the artifact is dropped with no error at either end.
const PostgresMetadataFileName = "pg_metadata.txt"

// pgDTMetadata is the receiver's data type for pg_metadata.txt. Classification
// is an exact string match, so any drift from the value the receiver registers
// drops the artifact silently at the far end - TestPostgresDataTypeConstant
// pins it.
const pgDTMetadata = "pgMeta"

// PostgresBloatFileName restates postgres.Bloat's own Artifact().FileName so
// callers outside that package have a constant to name;
// TestPostgresBloatFileNameMatchesTheArtifact keeps the two from drifting.
const PostgresBloatFileName = "pg_bloat.txt"

// pgDTBloat is the receiver's data type for pg_bloat.txt. Drift here drops the
// artifact silently, as it does for pgDTMetadata.
const pgDTBloat = "pgBloat"

// PostgresHealthFileName restates postgres.Health's own Artifact().FileName for
// the same reason PostgresBloatFileName does;
// TestPostgresHealthFileNameMatchesTheArtifact keeps the two from drifting.
const PostgresHealthFileName = "pg_health.txt"

// pgDTHealth is the receiver's data type for pg_health.txt. Drift here drops the
// artifact silently, as it does for pgDTMetadata. The filename's own
// registration on the bundle path is still outstanding.
const pgDTHealth = "pgHealth"

// pgSampledDataType is the receiver's data type for one of the window's
// artifacts, or empty when the server team has not assigned one yet. Seven
// artifacts are still unassigned, and an invented dt is dropped silently - the
// one failure mode that looks like success at both ends. While a value is empty
// the artifact is still written into the bundle and the upload is skipped with a
// message naming the reason.
func pgSampledDataType(artifact postgres.Artifact) string {
	switch artifact.Name {
	case "pg_metadata":
		return pgDTMetadata

	case "pg_bloat":
		return pgDTBloat

	case "pg_health":
		return pgDTHealth
	}

	return ""
}

// PostgresCapture runs the capture window and every artifact collected inside
// it. It is the run's only database task.
//
// Named for the mechanism rather than for one artifact: pg_metadata.txt,
// pg_health.txt and pg_bloat.txt register as collectors here rather than
// becoming tasks of their own, and pg_replication.txt and pg_capacity.txt will
// join them the same way. That is what keeps one run to one connection however
// many artifacts it grows.
//
// Target is a pointer so %v, %+v and %#v route through config.Postgres's String
// and GoString, which redact the password - WrapRun formats a failing task with
// %#v into an agent log that is itself uploaded.
type PostgresCapture struct {
	Capture
	Target *config.Postgres

	// mu guards cancel, which Run sets and Kill reads from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes every artifact, and uploads each under its own
// dt.
//
// Only a file-I/O failure returns a non-nil error. A refused connection, a
// denied probe or a database at max_connections are successful captures of a
// failure: WrapRun overwrites Result.Msg for a non-nil error, which would bury
// the connect_error the artifacts exist to record.
func (p *PostgresCapture) Run() (Result, error) {
	if p.Target == nil {
		// Defensive: spawned only for a configured block. A result rather than
		// an error keeps a wiring mistake out of WrapRun's %#v of the task.
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

		// Cheapest first: all three share exactly one tick, t0, and a shared
		// tick runs them in registration order. Health reads one view, metadata
		// runs three catalogue reads, and bloat's size functions stat every
		// relation's files - so the order buys t0 and nothing after it, since
		// bloat still holds the connection for its own sample.
		Collectors: []postgres.Collector{
			postgres.Health{},
			metadata,
			postgres.Bloat{},
		},
	}

	results := window.Run(ctx)

	// Read once the window has closed, which races nothing because Window.Run is
	// synchronous - and read into a value rather than held on the task: Metadata
	// carries no password where the collector holds the target, and WrapRun
	// formats a failing task with %#v into an agent log that is itself uploaded.
	return p.uploadArtifacts(results, metadata.Collected())
}

// Kill cancels the window.
//
// Overridden rather than inherited: Capture.Kill returns nil when Cmd is nil,
// so without this the capture would ignore teardown and hold the run open for
// the whole window. Nothing in the binary calls it yet - this is the contract a
// teardown path will need the day one exists.
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

// captureDuration is the configured window. Validate has already defaulted and
// clamped it, so a nil here means a block that never went through validation.
func (p *PostgresCapture) captureDuration() time.Duration {
	if p.Target.CaptureDuration == nil {
		return config.DefaultPostgresCaptureDuration
	}

	return p.Target.CaptureDuration.Duration()
}

// uploadArtifacts transmits each artifact and summarises the lot, keeping each
// capture summary in front of whatever its transmission had to say.
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
		// The one failure the artifact cannot record about itself, and so the
		// only one that becomes this task's error.
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

// postgresArtifactSummary is the run-log line for one artifact.
//
// pg_metadata.txt's is a reading rather than a count: what an operator wants
// from that line is which capture mode the run got - it decides which artifacts
// are even possible - and, when a probe was denied, which one. "1/1 samples" is
// true and useless, and Collect never returns an error, so that count could
// never be anything but 1/1.
//
// The adapter is the layer allowed to know which artifact is which; the window
// deliberately is not, which is why this is here and not in ArtifactResult.
func postgresArtifactSummary(artifact postgres.ArtifactResult, collected postgres.Metadata) string {
	if artifact.Artifact.Name != "pg_metadata" {
		return postgresArtifactMessage(artifact)
	}

	// The collector never reached the server, so the window holds the only
	// account of why - and all three artifacts then report the same refusal.
	if artifact.Status == postgres.StatusConnectFailed {
		collected.ConnectError = artifact.Err
	}

	return postgresResultMessage(collected)
}

// postgresArtifactMessage says what the run log should show for one sampled
// artifact. Every value it interpolates was redacted by the window that
// produced it.
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

// postgresTarget narrows the config block to what the capture package needs,
// which is what keeps that package free of a config import.
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

// postgresResultMessage says what the run log should show for pg_metadata.txt,
// fed from what the collector read. Every value it interpolates has already had
// the password removed by the package that produced it.
func postgresResultMessage(metadata postgres.Metadata) string {
	if metadata.ConnectError != "" {
		return fmt.Sprintf("%s written; postgres connect failed: %s",
			PostgresMetadataFileName, metadata.ConnectError)
	}

	parts := []string{fmt.Sprintf("%s written (mode=%s)", PostgresMetadataFileName, metadata.CaptureMode)}

	// Named neutrally rather than as denials: a denied grant is the common
	// cause, but a statement timeout reaches here identically and calling that
	// a denial would send a support engineer after the wrong thing.
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
