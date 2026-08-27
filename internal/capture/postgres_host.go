package capture

import (
	"context"
	"strings"
	"time"

	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/logger"
)

// hostCloseMargin keeps the last reading inside the window: the collectors start
// a moment after it opens, so a full-window gap would land after the database
// artifacts have closed.
const hostCloseMargin = 5 * time.Second

// hostCollector pairs a collector with the channel its result arrives on.
type hostCollector struct {
	name string
	done chan Result
}

// hostSpan is the gap netstat and ps leave between readings.
func hostSpan(window time.Duration) time.Duration {
	span := window - hostCloseMargin
	if span < time.Second {
		return time.Second
	}

	return span
}

// startHostCapture runs the host collectors when the run established this machine
// is the database's, and records the skip otherwise: host files describe whichever
// machine ran the agent. The caller must drain what it returns.
func (p *PostgresCapture) startHostCapture(ctx context.Context, m postgres.Metadata, window time.Duration) []hostCollector {
	if m.DeclarationContradicted {
		logger.Warn().Msgf("postgres agentOnDbHost is set, but this run measured that the database "+
			"is on another machine (%s); host capture stays off - the measurement wins.",
			m.AgentOnDBHostReason)
	}

	if m.HostArtifacts != postgres.HostArtifactsCaptured {
		p.logHostCaptureSkipped(m)

		return nil
	}

	logger.Log("Confirmed this machine runs the database (%s); capturing host data over the %s window.",
		m.AgentOnDBHostBy, window)

	span := hostSpan(window)
	stop := ctx.Done()
	endpoint := p.Endpoint()

	// Only netstat and ps stretch to the window: both already take several labelled
	// readings, so widening the gap changes timing and nothing else. ps spreads its
	// three evenly, hence span/2. The rest are untouched - these files carry one dt
	// whichever capture wrote them, and their shape is not this feature's to change.
	vmstat := &VMStat{}

	return []hostCollector{
		{"netstat", GoCapture(endpoint, WrapRun(&NetStat{sleepBetweenCaptures: span, stop: stop}))},
		{"ps", GoCapture(endpoint, WrapRun(&PS{sleepBetweenCaptures: span / 2, stop: stop}))},
		{"top", GoCapture(endpoint, WrapRun(&Top{}))},
		{"vmstat", GoCapture(endpoint, WrapRun(vmstat))},
		{"df", GoCapture(endpoint, WrapRun(&Disk{}))},

		// dmesg waits for vmstat as an application capture has it: no schedule of
		// its own to keep.
		{"dmesg", GoCapture(endpoint, WrapRun(&DMesg{}), vmstat)},
		{"kernel", GoCapture(endpoint, WrapRun(&Kernel{}))},
	}
}

// logHostCaptureSkipped states the skip and, where one exists, the deployment
// change that would lift it. Informational, not a warning: nothing failed.
func (p *PostgresCapture) logHostCaptureSkipped(m postgres.Metadata) {
	logger.Log("Host capture skipped: this run could not establish that this machine runs the "+
		"database (agent_on_db_host=%s, reason=%s).", m.AgentOnDBHost, m.AgentOnDBHostReason)

	if hint := postgres.HostCaptureHint(m.AgentOnDBHostReason); hint != "" {
		logger.Log("Host capture: %s", hint)
	}
}

// waitForHostCapture drains every collector, on every path: the channels are
// unbuffered, so an unreceived result blocks its goroutine forever.
func waitForHostCapture(collectors []hostCollector) (messages []string, ok bool) {
	ok = true

	for _, collector := range collectors {
		result := <-collector.done

		if !result.Ok {
			ok = false
		}

		messages = append(messages, collector.name+": "+result.Msg)
	}

	return messages, ok
}

// withHostCapture folds the collectors' messages into the capture's result.
func withHostCapture(result Result, messages []string, ok bool) Result {
	if len(messages) == 0 {
		return result
	}

	result.Msg = strings.Join(append([]string{result.Msg}, messages...), " | ")
	result.Ok = result.Ok && ok

	return result
}
