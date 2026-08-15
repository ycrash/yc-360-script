package postgres

import (
	"context"
	"io"
)

// Timeouts copies every statement timeout, lock timeout and idle-in-transaction termination logged, verbatim.
type Timeouts struct {
	tail logTail
}

// NewTimeouts constructs the collector.
func NewTimeouts() *Timeouts {
	return &Timeouts{tail: newLogTail("pg_timeouts", timeoutMatch)}
}

func (*Timeouts) Artifact() Artifact {
	return Artifact{
		Name:     "pg_timeouts",
		FileName: "pg_timeouts.txt",
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,

		SampleBudget: LogDrainBudget,
	}
}

func (t *Timeouts) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return t.tail.sample(ctx, q, w, s)
}

func (t *Timeouts) WriteEpilogue(w io.Writer, s SampleContext) error {
	return t.tail.writeEpilogue(w, s)
}

// 57014 and 55P03 are shared with other events (cancellation, NOWAIT), so both are paired with
// their message. 25P03 stands alone and is always one line: it fires with no active statement.
var timeoutMatch = eventMatch{
	sqlstate: []string{"57014", "55P03", "25P03"},
	message: []string{
		"canceling statement due to statement timeout",
		"canceling statement due to lock timeout",
		"terminating connection due to idle-in-transaction timeout",
	},
	paired: []string{"57014", "55P03"},
}
