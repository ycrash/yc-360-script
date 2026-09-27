package postgres

import (
	"context"
	"io"
)

// Errors copies every entry the server logged at ERROR, FATAL or PANIC during the
// window, less what pg_deadlocks.txt and pg_timeouts.txt copy, with the statements and
// the values an entry quotes replaced.
type Errors struct {
	tail logTail
}

// NewErrors constructs the collector.
func NewErrors() *Errors {
	tail := newLogTail("pg_errors", errorMatch)
	tail.redaction = &logRedaction{}

	return &Errors{tail: tail}
}

func (*Errors) Artifact() Artifact {
	return Artifact{
		Name:     "pg_errors",
		FileName: "pg_errors.txt",
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,

		SampleBudget: LogDrainBudget,
	}
}

func (e *Errors) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return e.tail.sample(ctx, q, w, s)
}

func (e *Errors) WriteClosing(w io.Writer, s SampleContext) error {
	return e.tail.writeClosing(w, s)
}

// errorMatch takes every entry logged at ERROR, FATAL or PANIC, with the rest of its
// event, less what pg_deadlocks.txt and pg_timeouts.txt already copy: no event is
// written twice. The levels are the English keywords every tail reads.
var errorMatch = eventMatch{
	severity: []string{"ERROR", "FATAL", "PANIC"},
	exclude:  []eventMatch{deadlockMatch, timeoutMatch},
}
