package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"yc-agent/internal/capture/dbcore"
	"yc-agent/internal/capture/executils"
	"yc-agent/internal/capture/mysql"
	"yc-agent/internal/config"
	"yc-agent/internal/logger"
)

// MySQLMetadataFileName is the first MySQL artifact. my_* dt values are proposed
// ahead of the receiver, which silently drops an unknown dt, so a reported
// success is not proof of arrival. An uploading run keeps no copy unless
// -onlyCapture or -d=false.
const MySQLMetadataFileName = "my_metadata.txt"

// mySampledDataType returns "" for an artifact with no dt; the caller skips its upload.
func mySampledDataType(artifact mysql.Artifact) string {
	switch artifact.Name {
	case "my_metadata":
		return "myMeta"
	case "my_sessions":
		return "mySessions"
	case "my_lock_waits":
		return "myLockWaits"
	case "my_deadlocks":
		return "myDeadlocks"
	case "my_timeouts":
		return "myTimeouts"
	case "my_errors":
		return "myErrors"
	case "my_slow_queries":
		return "mySlowQueries"
	case "my_prepared_statements":
		return "myPreparedStatements"
	case "my_explain":
		return "myExplain"
	case "my_table_stats":
		return "myTableStats"
	case "my_replication":
		return "myReplication"
	case "my_memory":
		return "myMemory"
	case "my_capacity":
		return "myCapacity"
	case "my_buffer_pool":
		return "myBufferPool"
	case "my_storage":
		return "myStorage"
	case "my_nondefault_settings":
		return "mySettings"
	}

	return ""
}

// MySQLCapture runs every my_* collector, one connection per speed. Target is a
// pointer so %v routes through config.MySQL's redacting String.
type MySQLCapture struct {
	Capture
	Target *config.MySQL

	// RunCtx ends the window early, like Kill but also before Run starts. nil never ends.
	RunCtx context.Context

	// mu guards cancel, set by Run and read by Kill from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes and uploads every artifact, and runs host capture
// where allowed. Only file I/O errors are returned; a refused connection is a
// captured outcome.
func (c *MySQLCapture) Run() (Result, error) {
	if c.Target == nil {
		return Result{Msg: "skipped mysql capture window: no mysql block configured"}, nil
	}

	run := c.RunCtx
	if run == nil {
		run = context.Background()
	}

	ctx, cancel := context.WithCancel(run)
	defer cancel()

	c.setCancel(cancel)
	defer c.setCancel(nil)

	target := mysqlTarget(c.Target)

	metadata := mysql.NewMetadata(target, executils.SCRIPT_VERSION, time.Now(), c.Target.Explain)
	metadata.DeclareOnDBHost(c.Target.AgentOnDBHost)

	duration := c.captureDuration()

	// Three speeds from mysql.frequency: fast (sessions, locks), normal (counters),
	// expensive (whole-table reads, settings).
	normal := c.frequency()
	fast := config.FastFrequency(normal)
	expensive := config.ExpensiveFrequency(normal)

	var hostCollectors []hostCollector

	hostStarted := false

	metadata.AfterCollect(func(m mysql.Metadata) {
		warnMariaDB(m)

		hostStarted = true
		hostCollectors = c.startHostCapture(ctx, m, duration)
	})

	window := mysql.NewWindow(target, duration,
		mysql.Collectors(metadata, c.Target.Explain, fast, normal, expensive), "")

	results := window.Run(ctx)

	warnConnectFailures(results)

	collected := metadata.ResolveHostDecision()

	// Never reached the server: only the operator's declaration allows host capture.
	if !hostStarted {
		hostCollectors = c.startHostCapture(ctx, collected, duration)
	}

	hostMessages, hostOK := waitForHostCapture(hostCollectors)

	result, err := c.uploadArtifacts(results, collected)
	if err != nil {
		return result, err
	}

	return withHostCapture(result, hostMessages, hostOK), nil
}

// warnMariaDB warns that the target is MariaDB; the run goes on.
func warnMariaDB(m mysql.Metadata) {
	if m.MySQLVariant != mysql.VariantMariaDB {
		return
	}

	logger.Warn().Msgf("The mysql: target is MariaDB %s, which the MySQL capture does not support: "+
		"my_metadata.txt records the server, and the other my_* files what it refused or lacks.", m.Version)
}

// warnConnectFailures logs each distinct connect failure once.
func warnConnectFailures(results []mysql.ArtifactResult) {
	files := map[string]int{}

	var failures []string

	for _, artifact := range results {
		if artifact.Status != mysql.StatusConnectFailed {
			continue
		}

		if files[artifact.Err] == 0 {
			failures = append(failures, artifact.Err)
		}

		files[artifact.Err]++
	}

	for _, failure := range failures {
		logger.Warn().Msgf("mysql connect failed: %s (%d of %d files record connect_failed)",
			failure, files[failure], len(results))
	}
}

// Kill overrides Capture.Kill, which ignores a nil Cmd.
func (c *MySQLCapture) Kill() error {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	return nil
}

func (c *MySQLCapture) setCancel(cancel context.CancelFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cancel = cancel
}

func (c *MySQLCapture) captureDuration() time.Duration {
	if c.Target.CaptureDuration == nil {
		return unvalidatedCaptureDuration
	}

	return c.Target.CaptureDuration.Duration()
}

func (c *MySQLCapture) frequency() time.Duration {
	if c.Target.Frequency == nil || c.Target.Frequency.Duration() <= 0 {
		return config.DefaultFrequency
	}

	return c.Target.Frequency.Duration()
}

// startHostCapture runs host collectors on the database's machine, else records the skip.
func (c *MySQLCapture) startHostCapture(ctx context.Context, m mysql.Metadata, window time.Duration) []hostCollector {
	if m.DeclarationContradicted {
		logger.Warn().Msgf("mysql agentOnDbHost is set, but this run measured that the database "+
			"is on another machine (%s); host capture stays off - the measurement wins.",
			m.AgentOnDBHostReason)
	}

	if m.HostArtifacts != dbcore.HostArtifactsCaptured {
		logger.Log("Host capture skipped: this run could not establish that this machine runs the "+
			"database (agent_on_db_host=%s, reason=%s).", m.AgentOnDBHost, m.AgentOnDBHostReason)

		return nil
	}

	logger.Log("Confirmed this machine runs the database (%s); capturing host data over the %s window.",
		m.AgentOnDBHostBy, window)

	return runHostCollectors(ctx, c.Endpoint(), window)
}

func (c *MySQLCapture) uploadArtifacts(artifacts []mysql.ArtifactResult, collected mysql.Metadata) (Result, error) {
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

		summary := mysqlArtifactSummary(artifact, collected)

		dt := mySampledDataType(artifact.Artifact)
		if dt == "" {
			messages = append(messages, summary+"; not uploaded: dt value not yet assigned")
			ok = false

			continue
		}

		msg, uploaded := PostData(c.Endpoint(), dt, artifact.File)
		if !uploaded {
			ok = false
		}

		messages = append(messages, summary+"; "+msg)
	}

	if ioErr != nil {
		return Result{}, fmt.Errorf("failed to capture mysql artifacts: %w", ioErr)
	}

	return Result{Msg: strings.Join(messages, " | "), Ok: ok}, nil
}

// mysqlArtifactSummary gives my_metadata.txt a reading rather than a sample count.
func mysqlArtifactSummary(artifact mysql.ArtifactResult, collected mysql.Metadata) string {
	if artifact.Artifact.Name != "my_metadata" {
		return mysqlArtifactMessage(artifact)
	}

	if artifact.Status == mysql.StatusConnectFailed {
		return fmt.Sprintf("%s written; mysql connect failed: %s", MySQLMetadataFileName, artifact.Err)
	}

	parts := []string{fmt.Sprintf("%s written (mysql_variant=%s, log_access=%s, agent_on_db_host=%s)",
		MySQLMetadataFileName, collected.MySQLVariant, collected.LogAccess, collected.AgentOnDBHost)}

	// MariaDB lacks performance_schema tables the other files read; say so.
	if collected.MySQLVariant == mysql.VariantMariaDB {
		parts = append(parts, "MariaDB is not supported by the MySQL capture; the other my_* files record refusals")
	}

	for _, probe := range []struct{ name, err string }{
		{"server facts query failed", collected.ServerFactsError},
		{"settings query failed", collected.SettingsError},
		{"capabilities query failed", collected.CapabilitiesError},
		{"privileges query failed", collected.PrivilegesError},
	} {
		if probe.err != "" {
			parts = append(parts, probe.name+": "+probe.err)
		}
	}

	return strings.Join(parts, "; ")
}

// mysqlArtifactMessage interpolates only values the window already redacted.
func mysqlArtifactMessage(artifact mysql.ArtifactResult) string {
	summary := fmt.Sprintf("%s written (%d/%d samples)",
		artifact.Artifact.FileName, artifact.SamplesWritten, artifact.SamplesExpected)
	if artifact.SamplesSkipped > 0 {
		summary = fmt.Sprintf("%s written (%d/%d samples, %d skipped as overdue)",
			artifact.Artifact.FileName, artifact.SamplesWritten, artifact.SamplesExpected, artifact.SamplesSkipped)
	}

	switch artifact.Status {
	case mysql.StatusConnectFailed:
		return summary + "; mysql connect failed: " + artifact.Err

	case mysql.StatusCancelled:
		return summary + "; window cancelled"

	case mysql.StatusDeadlineExceeded:
		return summary + "; window deadline exceeded"

	case mysql.StatusPartial:
		if artifact.Err == "" {
			return summary
		}

		return summary + "; last sample error: " + artifact.Err

	case mysql.StatusConnectionLost:
		return summary + "; connection lost: " + artifact.Err
	}

	return summary
}

// mysqlTarget keeps the capture package free of a config import.
func mysqlTarget(my *config.MySQL) mysql.Target {
	settings := mysql.TLS{
		Disabled: !my.TLSEnabled(),
		Verify:   my.TLSVerified(),
	}

	if my.TLS != nil {
		settings.CAFile = my.TLS.CAFile
		settings.ServerName = my.TLS.ServerName
	}

	return mysql.Target{
		ID:       my.ID,
		Host:     my.Host,
		Port:     my.Port,
		Database: my.Database,
		Username: my.Username,
		Password: my.Password,
		TLS:      settings,
	}
}
