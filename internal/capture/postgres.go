package capture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// PostgresMetadataFileName is the artifact this capture writes into the bundle.
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

// pgSampledDataType is the receiver's data type for one sampled artifact, or
// empty when the server team has not assigned one yet. Seven artifacts are still
// unassigned, and an invented dt is dropped silently - the one failure mode that
// looks like success at both ends. While a value is empty the artifact is still
// written into the bundle and the upload is skipped with a message naming the
// reason.
func pgSampledDataType(artifact postgres.Artifact) string {
	switch artifact.Name {
	case "pg_bloat":
		return pgDTBloat

	case "pg_health":
		return pgDTHealth
	}

	return ""
}

// PostgresMetadata captures what a run targeted in PostgreSQL and what happened
// to it. The artifact is written whether or not the connection succeeds: a
// capture that reports a refused connection is the only record of intent a run
// leaves behind.
//
// Target is a pointer so %v, %+v and %#v route through config.Postgres's String
// and GoString, which redact the password - WrapRun formats a failing task with
// %#v into an agent log that is itself uploaded.
//
// There is no output directory: FullCapture has already changed into the
// capture directory.
type PostgresMetadata struct {
	Capture
	Target *config.Postgres
}

// Run writes the artifact and uploads it.
//
// Only an I/O failure on the file returns a non-nil error. A refused
// connection, a denied probe or a database at max_connections are successful
// captures of a failure: WrapRun overwrites Result.Msg for a non-nil error,
// which would bury the connect_error the file exists to record.
func (p *PostgresMetadata) Run() (Result, error) {
	if p.Target == nil {
		// Defensive: spawned only for a configured block. A result rather than
		// an error keeps a wiring mistake out of WrapRun's %#v of the task.
		return Result{Msg: "skipped postgres metadata capture: no postgres block configured"}, nil
	}

	file, metadata, err := p.CaptureToFile()
	if err != nil {
		return Result{}, fmt.Errorf("failed to capture postgres metadata: %w", err)
	}
	defer file.Close()

	return p.UploadCapturedFile(file, metadata), nil
}

// CaptureToFile writes pg_metadata.txt in two passes: the target block is
// written and synced before the connection is attempted, so every failure path
// leaves a file - including the process being killed mid-connect.
func (p *PostgresMetadata) CaptureToFile() (*os.File, postgres.Metadata, error) {
	target := postgresTarget(p.Target)

	agentNow := time.Now()
	metadata := postgres.Metadata{
		AgentTS:            agentNow,
		AgentTSAtClockRead: agentNow,
		YC360Version:       executils.SCRIPT_VERSION,
		TargetHost:         target.Host,
		TargetPort:         target.Port,
		TargetDatabase:     target.Database,
		TargetUsername:     target.Username,
		TargetSSLMode:      target.SSLMode,
		CaptureMode:        postgres.ModeUnknown,
	}

	file, err := os.Create(PostgresMetadataFileName)
	if err != nil {
		return nil, metadata, fmt.Errorf("failed to create %s: %w", PostgresMetadataFileName, err)
	}

	if err := writePostgresTargetBlock(file, metadata); err != nil {
		file.Close()
		return nil, metadata, err
	}

	metadata = collectPostgresMetadata(target, metadata)

	if err := postgres.WriteResult(file, metadata); err != nil {
		file.Close()
		return nil, metadata, fmt.Errorf("failed to write %s: %w", PostgresMetadataFileName, err)
	}
	syncPostgresArtifact(file)

	return file, metadata, nil
}

// UploadCapturedFile transmits the artifact, keeping the capture summary in
// front of whatever the transmission had to say. Ok reports transmission, so it
// is false in -onlyCapture mode - the artifact is in the bundle either way.
func (p *PostgresMetadata) UploadCapturedFile(file *os.File, metadata postgres.Metadata) Result {
	summary := postgresResultMessage(metadata)

	msg, ok := PostData(p.Endpoint(), pgDTMetadata, file)

	return Result{
		Msg: summary + "; " + msg,
		Ok:  ok,
	}
}

// writePostgresTargetBlock writes the first pass and puts it on disk before the
// caller goes near the network.
func writePostgresTargetBlock(file *os.File, metadata postgres.Metadata) error {
	if err := postgres.WriteTarget(file, metadata); err != nil {
		return fmt.Errorf("failed to write %s: %w", PostgresMetadataFileName, err)
	}

	syncPostgresArtifact(file)

	return nil
}

// syncPostgresArtifact flushes the artifact, logging a failure rather than
// failing the capture: the bytes are written either way.
func syncPostgresArtifact(file *os.File) {
	if err := file.Sync(); err != nil {
		logger.Log("warning: failed to sync %s: %v", PostgresMetadataFileName, err)
	}
}

// collectPostgresMetadata connects and records what the server had to say. A
// failed connection is recorded in the returned value and never returned as an
// error - that is the whole point of the artifact.
func collectPostgresMetadata(target postgres.Target, metadata postgres.Metadata) postgres.Metadata {
	// Deliberately shorter than the worst case the per-statement deadlines allow:
	// a truncated statement records its own error and the artifact still
	// completes.
	ctx, cancel := context.WithTimeout(context.Background(), postgres.ModuleDeadline)
	defer cancel()

	conn, err := postgres.Connect(ctx, target)
	if err != nil {
		metadata.ConnectError = postgres.ConnectErrorText(err, target)
		return metadata
	}
	defer closePostgresConn(conn)

	// Collect returns a fresh value rather than filling this one in, so the
	// version stamped before the connection has to be carried across.
	collected := postgres.Collect(ctx, conn, target, metadata.AgentTS)
	collected.YC360Version = metadata.YC360Version

	return collected
}

// closePostgresConn closes the connection on a context of its own: the module
// deadline may already have expired, and closing under a cancelled context
// abandons the socket and leaves the server to time the backend out.
func closePostgresConn(conn *postgres.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), postgres.ConnectTimeout)
	defer cancel()

	if err := conn.Close(ctx); err != nil {
		logger.Log("warning: failed to close the postgres connection: %v", err)
	}
}

// PostgresSampler runs the capture window and every artifact collected inside
// it.
//
// Named for the mechanism rather than for one artifact: pg_health.txt,
// pg_replication.txt and pg_capacity.txt register as collectors here rather
// than becoming tasks of their own, which keeps one run to one connection
// however many sampled artifacts it grows.
//
// Target is a pointer for the same reason PostgresMetadata's is.
type PostgresSampler struct {
	Capture
	Target *config.Postgres

	// mu guards cancel, which Run sets and Kill reads from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes every artifact, and uploads each under its own
// dt.
//
// Only a file-I/O failure returns a non-nil error, for the reason
// PostgresMetadata.Run gives.
func (p *PostgresSampler) Run() (Result, error) {
	if p.Target == nil {
		// Defensive: spawned only for a configured block. A result rather than
		// an error keeps a wiring mistake out of WrapRun's %#v of the task.
		return Result{Msg: "skipped postgres capture window: no postgres block configured"}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.setCancel(cancel)
	defer p.setCancel(nil)

	window := &postgres.Window{
		Target:   postgresTarget(p.Target),
		Duration: p.captureDuration(),

		// Cheapest first: the two share exactly one tick, t0, and the first
		// registered runs there first. It buys that sample and nothing after
		// it - bloat still holds the connection for its own, so on a large
		// schema health's next tick or two run late.
		Collectors: []postgres.Collector{postgres.Health{}, postgres.Bloat{}},
	}

	return p.uploadArtifacts(window.Run(ctx))
}

// Kill cancels the window.
//
// Overridden rather than inherited: Capture.Kill returns nil when Cmd is nil,
// so without this the sampler would ignore teardown and hold the run open for
// the whole window. Nothing in the binary calls it yet - this is the contract a
// teardown path will need the day one exists.
func (p *PostgresSampler) Kill() error {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	return nil
}

func (p *PostgresSampler) setCancel(cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancel = cancel
}

// captureDuration is the configured window. Validate has already defaulted and
// clamped it, so a nil here means a block that never went through validation.
func (p *PostgresSampler) captureDuration() time.Duration {
	if p.Target.CaptureDuration == nil {
		return config.DefaultPostgresCaptureDuration
	}

	return p.Target.CaptureDuration.Duration()
}

// uploadArtifacts transmits each artifact and summarises the lot, keeping each
// capture summary in front of whatever its transmission had to say.
func (p *PostgresSampler) uploadArtifacts(artifacts []postgres.ArtifactResult) (Result, error) {
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

		summary := postgresSamplerMessage(artifact)

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
		return Result{}, fmt.Errorf("failed to capture postgres sampled artifacts: %w", ioErr)
	}

	return Result{Msg: strings.Join(messages, " | "), Ok: ok}, nil
}

// postgresSamplerMessage says what the run log should show for one artifact.
// Every value it interpolates was redacted by the window that produced it.
func postgresSamplerMessage(artifact postgres.ArtifactResult) string {
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

// postgresResultMessage says what the run log should show for this capture.
// Every value it interpolates has already had the password removed by the
// package that produced it.
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
