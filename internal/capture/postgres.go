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

// PostgresMetadataFileName and the eight below must equal
// YCrashDataType.fromAgentFileName()'s agentFileName exactly, or a -onlyCapture
// bundle's artifact is dropped with no error at either end.
const PostgresMetadataFileName = "pg_metadata.txt"

// pgDTMetadata and the dt consts below are an exact-match contract with the
// receiver; drift drops the artifact silently.
const pgDTMetadata = "pgMeta"

const PostgresBloatFileName = "pg_bloat.txt"

const pgDTBloat = "pgBloat"

const PostgresHealthFileName = "pg_health.txt"

const pgDTHealth = "pgHealth"

const PostgresCapacityFileName = "pg_capacity.txt"

const pgDTCapacity = "pgCapacity"

const PostgresReplicationFileName = "pg_replication.txt"

const pgDTReplication = "pgReplication"

const PostgresSessionsFileName = "pg_sessions.txt"

const pgDTSessions = "pgSessions"

const PostgresSlowQueriesFileName = "pg_slow_queries.txt"

const pgDTSlowQueries = "pgSlowQueries"

const PostgresExplainFileName = "pg_explain.txt"

const pgDTExplain = "pgExplain"

const PostgresDeadlocksFileName = "pg_deadlocks.txt"

const pgDTDeadlocks = "pgDeadlocks"

const PostgresTimeoutsFileName = "pg_timeouts.txt"

const pgDTTimeouts = "pgTimeouts"

// pgSampledDataType returns "" for an artifact with no assigned dt: an invented
// value would upload and drop silently, so the caller writes the artifact but
// skips the upload with an explicit reason instead.
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

	case "pg_replication":
		return pgDTReplication

	case "pg_sessions":
		return pgDTSessions

	case "pg_slow_queries":
		return pgDTSlowQueries

	case "pg_deadlocks":
		return pgDTDeadlocks

	case "pg_timeouts":
		return pgDTTimeouts

	case "pg_explain":
		return pgDTExplain
	}

	return ""
}

// PostgresCapture runs every postgres artifact as one collector each over a
// single connection. Target is a pointer so %v/%+v/%#v route through
// config.Postgres's String/GoString, which redact the password.
type PostgresCapture struct {
	Capture
	Target *config.Postgres

	// mu guards cancel, set by Run and read by Kill from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes every artifact, and uploads each under its own dt.
// Only a file-I/O failure returns a non-nil error: a refused connection is a
// successful capture of a failure, and WrapRun overwrites Result.Msg on error,
// which would bury the connect_error the artifacts exist to record.
func (p *PostgresCapture) Run() (Result, error) {
	if p.Target == nil {
		return Result{Msg: "skipped postgres capture window: no postgres block configured"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.setCancel(cancel)
	defer p.setCancel(nil)

	target := postgresTarget(p.Target)
	// The explain mode rides construction so it reaches the pre-connect target block,
	// which is written before dialling and survives a refused connection.
	metadata := postgres.NewMetadata(target, executils.SCRIPT_VERSION, time.Now(), p.explainMode())

	// Shared, not two collectors: Explain ranks the endpoints this one retains, and
	// never re-runs the statement behind them.
	slowQueries := postgres.NewSlowQueries()

	window := &postgres.Window{
		Target:   target,
		Duration: p.captureDuration(),

		// Registration order is sampling order on the shared tick, not a timing
		// guarantee. Log tails go first so from_offset is set before other
		// collectors' statements reach the log; otherwise cheapest reads go first
		// at t0, and at the closing tick (capacity, bloat, slow queries) the
		// reading with no second chance goes first.
		//
		// pg_explain goes last on both counts: at the close it reads slowQueries' second
		// sample, and at t0 its log tail then opens past the agent's own first plans.
		Collectors: []postgres.Collector{
			postgres.NewDeadlocks(),
			postgres.NewTimeouts(),
			postgres.Sessions{},
			postgres.Health{},
			postgres.Replication{},
			metadata,
			postgres.Capacity{},
			postgres.Bloat{},
			slowQueries,
			postgres.NewExplain(p.explainMode(), slowQueries),
		},
	}

	results := window.Run(ctx)

	return p.uploadArtifacts(results, metadata.Collected())
}

// Kill overrides Capture.Kill, which returns nil when Cmd is nil and would
// otherwise ignore teardown and hold the run open.
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

// captureDuration returns the configured window, defaulting a nil duration
// (a config block that never went through Validate).
func (p *PostgresCapture) captureDuration() time.Duration {
	if p.Target.CaptureDuration == nil {
		return config.DefaultPostgresCaptureDuration
	}

	return p.Target.CaptureDuration.Duration()
}

// explainMode passes config's own value through: the postgres package does not import
// internal/config, and Validate has already rejected everything but the two spellings.
func (p *PostgresCapture) explainMode() string {
	if p.Target == nil {
		return ""
	}

	return p.Target.Explain
}

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

// postgresArtifactSummary gives pg_metadata.txt a reading rather than a sample
// count: its Once() schedule makes "1/1 samples" true and useless.
func postgresArtifactSummary(artifact postgres.ArtifactResult, collected postgres.Metadata) string {
	if artifact.Artifact.Name != "pg_metadata" {
		return postgresArtifactMessage(artifact)
	}

	// The window holds the only account of why: the collector never reached the server.
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

	parts := []string{fmt.Sprintf("%s written (log_access=%s)", PostgresMetadataFileName, metadata.LogAccess)}

	// Named neutrally, not as denials: a statement timeout reaches here identically.
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
