package postgres

import (
	"context"
	"io"
)

// CheckpointLog copies every checkpoint completion line the server logged during
// the window, verbatim: the buffers written, the WAL files recycled, and the
// write, sync and total times. The counters in pg_capacity.txt say how many
// checkpoints ran; this says what each one cost.
type CheckpointLog struct {
	tail logTail
}

// NewCheckpointLog constructs the collector.
func NewCheckpointLog() *CheckpointLog {
	return &CheckpointLog{tail: newLogTail("pg_checkpoint_log", checkpointMatch)}
}

func (*CheckpointLog) Artifact() Artifact {
	return Artifact{
		Name:     "pg_checkpoint_log",
		FileName: "pg_checkpoint_log.txt",
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,

		SampleBudget: LogDrainBudget,
	}
}

func (c *CheckpointLog) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return c.tail.sample(ctx, q, w, s)
}

func (c *CheckpointLog) WriteClosing(w io.Writer, s SampleContext) error {
	return c.tail.writeClosing(w, s)
}

// A checkpoint's completion line is LOG severity, and every LOG line carries the
// one SQLSTATE 00000 (successful_completion) in csvlog and jsonlog: no code names
// the event, so the message decides in every format - the shape pg_explain.txt's
// tail already uses for auto_explain - and matched_by= says so. lc_messages can
// translate the message, the blind spot the two siblings have on stderr only;
// this one has it everywhere, and a translated cluster yields matched=0 rather
// than a mis-attributed line. "checkpoint starting:" is deliberately not matched,
// and nor is a standby's "restartpoint complete:": the spec names the completion
// line, whose numbers are the finding. The line is one line, so none of the
// multi-line event handling applies. The server writes it only under
// log_checkpoints = on, which pg_metadata.txt records.
var checkpointMatch = eventMatch{
	sqlstate: []string{"00000"},
	paired:   []string{"00000"},
	message:  []string{"checkpoint complete:"},
}
