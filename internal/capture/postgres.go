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

// PostgresMetadataFileName and the twelve below must equal
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

// PostgresIndexUsageFileName has no dt constant beside it: dt=pgIndexUsage is
// proposed to the server team and not yet assigned, so pgSampledDataType returns
// "" for the artifact and the run writes it into the bundle without uploading it.
const PostgresIndexUsageFileName = "pg_index_usage.txt"

// PostgresTablespacesFileName, on the same terms: dt=pgTablespaces is proposed,
// not assigned.
const PostgresTablespacesFileName = "pg_tablespaces.txt"

// PostgresCheckpointLogFileName, on the same terms: dt=pgCheckpointLog is
// proposed, not assigned.
const PostgresCheckpointLogFileName = "pg_checkpoint_log.txt"

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

// Run opens the window, writes every artifact, uploads each under its own dt, and
// runs the host collectors where the run established that this machine is the
// database's.
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
	metadata.DeclareOnDBHost(p.Target.AgentOnDBHost)

	duration := p.captureDuration()

	// One cadence for every sampled artifact, postgres.frequency: the sampled
	// collectors below take it rather than each carrying its own constant.
	interval := p.frequency()

	// Written by the callback below and read once the window closes. Window.Run is
	// synchronous on this goroutine, so the two never overlap.
	var hostCollectors []hostCollector
	hostStarted := false

	// Host capture starts from inside the window, at the moment the probe's answer
	// is known, so its readings cover the same span as the database artifacts
	// rather than a burst before them.
	metadata.AfterCollect(func(m postgres.Metadata) {
		hostStarted = true
		hostCollectors = p.startHostCapture(ctx, m, duration)
	})

	// Shared, not two collectors: Explain walks the read this one offers each sample,
	// and never re-runs the statement behind it.
	slowQueries := postgres.NewSlowQueries()
	slowQueries.Interval = interval

	explain := postgres.NewExplain(p.explainMode(), slowQueries)
	explain.Interval = interval

	window := &postgres.Window{
		Target:   target,
		Duration: duration,

		// Registration order is sampling order on the shared tick, not a timing
		// guarantee. Log tails go first so from_offset is set before other
		// collectors' statements reach the log; then the cheap reads, then the
		// whole-table and whole-filesystem reads (capacity, bloat, index usage,
		// tablespaces, slow queries), so a tick that runs long is late with the
		// expensive reading rather than the cheap ones.
		//
		// pg_explain goes last on both counts: on every tick it walks slowQueries'
		// read of that tick, and at t0 its log tail then opens past the agent's own
		// first plans.
		Collectors: []postgres.Collector{
			postgres.NewDeadlocks(),
			postgres.NewTimeouts(),
			postgres.NewCheckpointLog(),
			postgres.Sessions{Interval: interval},
			postgres.Health{Interval: interval},
			postgres.Replication{Interval: interval},
			metadata,
			postgres.Capacity{Interval: interval},
			postgres.Bloat{Interval: interval},
			postgres.IndexUsage{Interval: interval},
			postgres.Tablespaces{Interval: interval},
			slowQueries,
			explain,
		},
	}

	results := window.Run(ctx)

	collected := metadata.ResolveHostDecision()

	// The window never reached the server, so the probe never ran and the operator's
	// declaration is the only thing that can authorise host capture. That is the
	// case it exists for: a database that is down cannot be asked which machine it
	// runs on, and it is down exactly when the host readings matter most.
	if !hostStarted {
		hostCollectors = p.startHostCapture(ctx, collected, duration)
	}

	hostMessages, hostOK := waitForHostCapture(hostCollectors)

	result, err := p.uploadArtifacts(results, collected)
	if err != nil {
		return result, err
	}

	return withHostCapture(result, hostMessages, hostOK), nil
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

// frequency returns the configured cadence, defaulting a nil or zero value (a
// config block that never went through Validate).
func (p *PostgresCapture) frequency() time.Duration {
	if p.Target.Frequency == nil || p.Target.Frequency.Duration() <= 0 {
		return config.DefaultPostgresFrequency
	}

	return p.Target.Frequency.Duration()
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

	case postgres.StatusConnectionLost:
		return summary + "; connection lost: " + artifact.Err
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

	// The two together: log_access is a permission, agent_on_db_host a location, and
	// a run can have either without the other.
	parts := []string{fmt.Sprintf("%s written (log_access=%s, agent_on_db_host=%s)",
		PostgresMetadataFileName, metadata.LogAccess, metadata.AgentOnDBHost)}

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
