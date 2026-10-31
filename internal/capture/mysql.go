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

// MySQLMetadataFileName is the first MySQL artifact, the rest named by their
// collectors. Every my_* artifact uploads under a dt value the agent proposed,
// following the postgres pattern, ahead of the receiver having them. A receiver
// that does not know a dt accepts the upload and throws the file away with no
// error at either end, so a reported success for these is not yet proof of
// arrival; the bundle carries the files regardless.
const MySQLMetadataFileName = "my_metadata.txt"

// mySampledDataType returns "" for an artifact with no dt at all: an invented value
// would upload and drop silently, so the caller writes the artifact but skips the
// upload with an explicit reason instead.
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

// MySQLCapture runs every my_* artifact as one collector each, over one connection
// per speed. Target is a pointer so %v/%+v/%#v route through config.MySQL's
// String/GoString, which redact the password.
type MySQLCapture struct {
	Capture
	Target *config.MySQL

	// RunCtx is the run's: its end stops the window early, as Kill does, and,
	// unlike Kill, also before Run has begun. nil never ends.
	RunCtx context.Context

	// mu guards cancel, set by Run and read by Kill from another goroutine.
	mu     sync.Mutex
	cancel context.CancelFunc
}

// Run opens the window, writes every artifact, uploads each under its own dt, and
// runs the host collectors where the run established that this machine is the
// database's. Only a file-I/O failure returns a non-nil error: a refused connection
// is a successful capture of a failure.
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

	// Three speeds derived from mysql.frequency, each a fixed property of the file:
	// session and lock state at fast, counters at normal, whole-table reads and
	// settings at expensive.
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

	// The window never reached the server, so the operator's declaration is the only
	// thing that can authorise host capture.
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

// warnMariaDB says at the window's start what the run reached: the run goes on, and
// the files it uploads record MariaDB's refusals.
func warnMariaDB(m mysql.Metadata) {
	if m.MySQLVariant != mysql.VariantMariaDB {
		return
	}

	logger.Warn().Msgf("The mysql: target is MariaDB %s, which the MySQL capture does not support: "+
		"my_metadata.txt records the server, and the other my_* files what it refused or lacks.", m.Version)
}

// warnConnectFailures says each distinct connect failure once: the run goes on to
// upload files that record only the failure, and its closing summary is INF. A
// window that connects nowhere returns at once, so this comes at the start.
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

// Kill overrides Capture.Kill, which returns nil when Cmd is nil and would
// otherwise ignore teardown and hold the run open.
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

// startHostCapture runs the host collectors when the run established this machine
// is the database's, and records the skip otherwise.
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

	// MariaDB is identified and nothing more: its performance_schema lacks tables the
	// other files read, so they fail and this says why.
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
