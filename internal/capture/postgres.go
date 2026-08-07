package capture

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"yc-agent/internal/capture/executils"
	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// PostgresMetadataFileName is the artifact this capture writes into the bundle.
// Database artifacts keep engine-specific prefixes: pg_ here, my_ and ora_ to
// follow.
const PostgresMetadataFileName = "pg_metadata.txt"

// pgDTMetadata is the receiver's data type for pg_metadata.txt, assigned by the
// server team (direction §1.2). Classification is an exact string match, so any
// drift from the value the receiver registers drops the artifact silently at
// the far end - TestPostgresDataTypeConstant pins it.
const pgDTMetadata = "pgMeta"

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
// leaves a file - including the process being killed mid-connect, a live
// possibility on the wedged hosts this capture runs against.
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
// front of whatever the transmission had to say.
//
// A capture that recorded a refused connection is uploaded like any other: the
// connect_error is the finding. Ok reports transmission, as it does for every
// other capture, so it is false in -onlyCapture mode - the artifact is in the
// bundle either way.
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

// syncPostgresArtifact flushes the artifact to disk, logging a failure rather
// than failing the capture: the bytes are written either way, and the sync is
// insurance against the process not surviving to the next pass.
func syncPostgresArtifact(file *os.File) {
	if err := file.Sync(); err != nil {
		logger.Log("warning: failed to sync %s: %v", PostgresMetadataFileName, err)
	}
}

// collectPostgresMetadata connects and records what the server had to say. A
// failed connection is recorded in the returned value and never returned as an
// error - that is the whole point of the artifact.
func collectPostgresMetadata(target postgres.Target, metadata postgres.Metadata) postgres.Metadata {
	// Deliberately shorter than the worst case the per-statement deadlines allow
	// (a 5s connect plus three 10s statements): a truncated statement records
	// its own error and the artifact still completes.
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
