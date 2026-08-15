package postgres

import (
	"context"
	"io"
)

// Deadlocks copies every deadlock report the server logged during the window, verbatim.
type Deadlocks struct {
	tail logTail
}

// NewDeadlocks constructs the collector.
func NewDeadlocks() *Deadlocks {
	return &Deadlocks{tail: newLogTail("pg_deadlocks", deadlockMatch)}
}

func (*Deadlocks) Artifact() Artifact {
	return Artifact{
		Name:     "pg_deadlocks",
		FileName: "pg_deadlocks.txt",
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,

		// Unused by Every-scheduled collectors; kept so the deadline is explicit here too.
		SampleBudget: LogDrainBudget,
	}
}

func (d *Deadlocks) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return d.tail.sample(ctx, q, w, s)
}

func (d *Deadlocks) WriteEpilogue(w io.Writer, s SampleContext) error {
	return d.tail.writeEpilogue(w, s)
}

// 40P01 (deadlock_detected) is unique, so csvlog/jsonlog match locale-independently on SQLSTATE.
// On stderr only the message is available, and lc_messages can translate it (matched_by=message).
var deadlockMatch = eventMatch{
	sqlstate: []string{"40P01"},
	message:  []string{"deadlock detected"},
}
