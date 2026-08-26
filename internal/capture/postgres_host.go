package capture

import (
	"context"
	"strings"
	"time"

	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/logger"
)

// hostCloseMargin keeps the closing host reading inside the capture window. The
// collectors start a moment after the window opens, so a full-window gap would
// put their second reading past the point where the database artifacts have
// already been closed and uploaded.
const hostCloseMargin = 5 * time.Second

// hostCollector pairs a collector with the channel its result arrives on.
type hostCollector struct {
	name string
	done chan Result
}

// hostSpan is how long the paired collectors leave between their two readings.
func hostSpan(window time.Duration) time.Duration {
	span := window - hostCloseMargin
	if span < time.Second {
		return time.Second
	}

	return span
}

// startHostCapture runs the host collectors when the run established that this
// machine is the database's, and records the skip otherwise. Host files describe
// whichever machine ran the agent, so on any other answer they would be a
// foreign machine's CPU, memory, disk and connection table filed under the
// database's name.
//
// It returns the collectors it started, which the caller must drain.
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

	// The four paired collectors read at both edges of the window: a df delta is
	// what makes "the filesystem is filling" a finding, and a second dmesg is what
	// catches a kill that happens during the window rather than before it. ps
	// spreads its three readings evenly, so span/2 puts the last one on the closing
	// edge. top and vmstat keep their own fixed span at the opening edge, unchanged
	// from every application capture. kernel is a single reading of settings that
	// do not move.
	//
	// dmesg does not wait for vmstat the way an application capture has it: here it
	// has a schedule of its own, and starting it twenty seconds late would push its
	// closing reading outside the window.
	return []hostCollector{
		{"netstat", GoCapture(endpoint, WrapRun(&NetStat{sleepBetweenCaptures: span, stop: stop}))},
		{"ps", GoCapture(endpoint, WrapRun(&PS{sleepBetweenCaptures: span / 2, stop: stop}))},
		{"df", GoCapture(endpoint, WrapRun(&Disk{sleepBetweenCaptures: span, stop: stop}))},
		{"dmesg", GoCapture(endpoint, WrapRun(&DMesg{sleepBetweenCaptures: span, stop: stop}))},
		{"top", GoCapture(endpoint, WrapRun(&Top{}))},
		{"vmstat", GoCapture(endpoint, WrapRun(&VMStat{}))},
		{"kernel", GoCapture(endpoint, WrapRun(&Kernel{}))},
	}
}

// logHostCaptureSkipped states the skip and, where one exists, the deployment
// change that would turn the answer into a yes. Informational rather than a
// warning: nothing failed, and on a managed service nothing can.
func (p *PostgresCapture) logHostCaptureSkipped(m postgres.Metadata) {
	logger.Log("Host capture skipped: this run could not establish that this machine runs the "+
		"database (agent_on_db_host=%s, reason=%s).", m.AgentOnDBHost, m.AgentOnDBHostReason)

	if hint := postgres.HostCaptureHint(m.AgentOnDBHostReason); hint != "" {
		logger.Log("Host capture: %s", hint)
	}
}

// waitForHostCapture drains every collector. It must run on every path: the
// result channels are unbuffered, so a result nobody receives leaves the
// collector's goroutine blocked on its send.
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

// withHostCapture folds the host collectors' messages into the database
// capture's own result.
func withHostCapture(result Result, messages []string, ok bool) Result {
	if len(messages) == 0 {
		return result
	}

	result.Msg = strings.Join(append([]string{result.Msg}, messages...), " | ")
	result.Ok = result.Ok && ok

	return result
}
