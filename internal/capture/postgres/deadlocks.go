package postgres

import (
	"context"
	"io"
)

// Deadlocks copies every deadlock report the server logged during the window, with its
// statements' text replaced: the processes, locks and transactions are kept.
type Deadlocks struct {
	tail logTail
}

// NewDeadlocks constructs the collector.
func NewDeadlocks() *Deadlocks {
	tail := newLogTail("pg_deadlocks", deadlockMatch)
	tail.redaction = &logRedaction{deadlockReport: true}

	return &Deadlocks{tail: tail}
}

func (*Deadlocks) Artifact() Artifact {
	return Artifact{
		Name:     "pg_deadlocks",
		FileName: "pg_deadlocks.txt",
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,

		// A tail delayed behind a slow read loses nothing: it reads on from its offset.
		Connection: ConnectionNormal,

		// Unused by Every-scheduled collectors; kept so the deadline is explicit here too.
		SampleBudget: LogDrainBudget,
	}
}

func (d *Deadlocks) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return d.tail.sample(ctx, q, w, s)
}

func (d *Deadlocks) WriteClosing(w io.Writer, s SampleContext) error {
	return d.tail.writeClosing(w, s)
}

// 40P01 (deadlock_detected) is unique, so csvlog/jsonlog match locale-independently on SQLSTATE.
// On stderr only the message is available, and lc_messages can translate it (matched_by=message).
var deadlockMatch = eventMatch{
	sqlstate: []string{"40P01"},
	message:  []string{"deadlock detected"},
}
