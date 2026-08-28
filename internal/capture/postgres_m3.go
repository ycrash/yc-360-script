package capture

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// postgresM3OutputPath is what the cycle directory holds between the reading and
// the upload. The directory is deleted when the cycle ends; nothing accumulates.
const postgresM3OutputPath = postgres.M3FileName

// pgDTM3 is provisional: the value is the agent's proposal, not yet confirmed by
// the receiver. It collides with none of the ten assigned above, so the worst case
// is a receiver that has no handler and drops the payload - visible in the reply
// this logs, and a one-line change if the receiver picks another name.
const pgDTM3 = "pgM3"

// PostgresM3 is the M3-mode database capture: one small reading per cycle, in
// place of the ten-file capture a one-shot run takes. It runs first in the cycle
// because its answer decides whether the host-scoped captures run at all. Target
// is a pointer so %v/%#v route through config.Postgres's redaction.
type PostgresM3 struct {
	Capture
	Target *config.Postgres

	// mu guards cancel, set by Run and read by Kill from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc

	// result is read after Run, on the cycle's own goroutine.
	result postgres.PollResult
}

// Run takes the reading, writes it and uploads it. Only a file-I/O failure returns
// an error: a database that cannot be reached is the reading this artifact exists
// to carry, and WrapRun would overwrite the message that says so.
func (p *PostgresM3) Run() (Result, error) {
	if p.Target == nil {
		return Result{Msg: "skipped postgres capture: no postgres block configured"}, nil
	}

	capturedFile, err := p.captureToFile()
	if err != nil {
		return Result{}, err
	}
	defer capturedFile.Close()

	if err := capturedFile.Sync(); err != nil {
		logger.Log("warning: failed to sync file: %v", err)
	}

	result := p.UploadCapturedFile(capturedFile)

	p.logReading(result.Ok)

	return result, nil
}

// captureToFile creates the output file and writes the reading to it.
func (p *PostgresM3) captureToFile() (*os.File, error) {
	file, err := os.Create(postgresM3OutputPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create output file: %w", err)
	}

	if err := p.captureOutput(file); err != nil {
		file.Close()

		return nil, err
	}

	return file, nil
}

// captureOutput takes the reading and renders it. It renders whole and writes
// once, so a failed write cannot leave half a payload for the receiver to parse.
func (p *PostgresM3) captureOutput(w io.Writer) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.setCancel(cancel)
	defer p.setCancel(nil)

	p.result = postgres.Poll(ctx, postgres.PollRequest{
		Target:           postgresTarget(p.Target),
		Runner:           runnerName(),
		Now:              time.Now(),
		DeclaredOnDBHost: p.Target.AgentOnDBHost,
	})

	var payload bytes.Buffer
	if err := postgres.WritePoll(&payload, p.result); err != nil {
		return fmt.Errorf("failed to render the postgres reading: %w", err)
	}

	_, err := w.Write(payload.Bytes())

	return err
}

// UploadCapturedFile uploads the captured file to the configured endpoint.
func (p *PostgresM3) UploadCapturedFile(file *os.File) Result {
	msg, ok := PostData(p.Endpoint(), pgDTM3, file)

	return Result{Msg: msg, Ok: ok}
}

// OnDBHost reports the poll's answer. It gates the top capture in the same cycle,
// so it is read after Run and never before.
func (p *PostgresM3) OnDBHost() bool { return p.result.OnDBHost() }

// Kill overrides Capture.Kill, which returns nil when Cmd is nil and would
// otherwise leave the poll running.
func (p *PostgresM3) Kill() error {
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	return nil
}

func (p *PostgresM3) setCancel(cancel context.CancelFunc) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.cancel = cancel
}

// logReading writes the one line every reading writes, then the failures worth a second.
func (p *PostgresM3) logReading(sent bool) {
	logger.Log("%s", p.result.LogLine(sent))

	if p.result.DeclarationContradicted {
		logger.Warn().Msgf("postgres agentOnDbHost is set, but this poll measured that the database "+
			"is on another machine (%s); host capture stays off - the measurement wins.",
			p.result.AgentOnDBHostReason)
	}

	if p.result.LogError != "" {
		logger.Log("pg_m3: %s: %s", p.result.HeartbeatError, p.result.ErrorDetail())
	}

	// The fix for the reason, where one exists. Informational: nothing failed.
	if hint := postgres.HostCaptureHint(p.result.AgentOnDBHostReason); hint != "" {
		logger.Log("pg_m3: %s", hint)
	}

	for _, note := range p.result.Notes {
		logger.Log("pg_m3: %s", note)
	}
}

// runnerName names the machine the runner-health and disk figures describe. An
// unreadable hostname leaves it empty rather than guessing: the server uses it to
// spot two runners polling one database, and a made-up name would hide that.
func runnerName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}

	return name
}
