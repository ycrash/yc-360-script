package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
)

// DBErrorsLogName is the file's name in the application-log stream: uploaded as
// dt=applog&logName=db_errors.log and bundled as N.appLogs.db_errors.log, so the
// server's log parser reads it as it reads an application's own log.
const DBErrorsLogName = "db_errors.log"

// errorMatch takes every entry logged at ERROR, FATAL or PANIC, with the rest of its
// event, less what pg_deadlocks.txt and pg_timeouts.txt already copy: no event is
// written twice. The levels are the English keywords every tail reads.
var errorMatch = eventMatch{
	severity: []string{"ERROR", "FATAL", "PANIC"},
	exclude:  []eventMatch{deadlockMatch, timeoutMatch},
}

// DBErrors copies every error the server logged during the window into db_errors.log,
// verbatim and in whichever format the tail reads, with no line of its own. What the
// other tails say in their block headers it says on pg_metadata.txt's closing block.
type DBErrors struct {
	fileName string
	tail     logTail

	// sampled: resolution has run at least once. read: a sample found the log open,
	// the difference between an empty file and no file.
	sampled bool
	read    bool

	format logFormat

	// written counts the file's bytes against MaxArtifactBytes; full is set by the
	// first event that did not fit.
	written int64
	full    bool

	matched         int
	kept            int
	dropped         int
	eventsTruncated int
	rotations       int
	fileTruncations int
	carryDropped    int
	partialEvents   int
	skippedBytes    int64
	resolvedLate    bool
}

// NewDBErrors writes into fileName, the bundle's name for db_errors.log.
func NewDBErrors(fileName string) *DBErrors {
	return &DBErrors{fileName: fileName, tail: newLogTail("pg_errors", errorMatch)}
}

func (d *DBErrors) Artifact() Artifact {
	return Artifact{
		Name:     "pg_errors",
		FileName: d.fileName,
		Scope:    "cluster",
		Schedule: Every(DefaultLogTailInterval),
		Format:   formatText,
		Bare:     true,

		SampleBudget: LogDrainBudget,
	}
}

// Sample errors only when the write fails: a log it cannot read is said on the closing
// block, not in the file.
func (d *DBErrors) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	d.sampled = true

	read, ok := d.tail.readSample(ctx, q, s)
	if !ok {
		return nil
	}

	d.read = true

	return d.write(w, read)
}

func (d *DBErrors) WriteClosing(w io.Writer, _ SampleContext) error {
	read, ok := d.tail.readDrain()
	if !ok {
		return nil
	}

	return d.write(w, read)
}

// write keeps the first events: once one would carry the file past MaxArtifactBytes,
// it and every event after it are counted as dropped rather than written.
func (d *DBErrors) write(w io.Writer, read *tailRead) error {
	d.count(read)

	var body bytes.Buffer

	for _, event := range read.events {
		if d.full || d.written+int64(len(event)) > MaxArtifactBytes {
			d.full = true
			d.dropped++

			continue
		}

		body.Write(event)
		d.written += int64(len(event))
		d.kept++
	}

	if body.Len() == 0 {
		return nil
	}

	_, err := w.Write(body.Bytes())

	return err
}

func (d *DBErrors) count(read *tailRead) {
	d.format = d.tail.source.format

	d.matched += read.matched
	d.eventsTruncated += read.eventsTruncated
	d.skippedBytes += read.skipped

	for _, hit := range []struct {
		happened bool
		count    *int
	}{
		{read.rotated, &d.rotations},
		{read.truncated, &d.fileTruncations},
		{read.carryDropped, &d.carryDropped},
		{read.partial, &d.partialEvents},
	} {
		if hit.happened {
			*hit.count++
		}
	}

	if read.resolvedLate {
		d.resolvedLate = true
	}
}

// LogRead is whether the log was ever open to this tail. Without it there is no file:
// an empty one would read as a window with no errors.
func (d *DBErrors) LogRead() bool { return d.read }

// Kept and Dropped are the events written and those past the size cap.
func (d *DBErrors) Kept() int { return d.kept }

func (d *DBErrors) Dropped() int { return d.dropped }

// LogAccess is the log as the tail last saw it, with the reason when it was not
// readable. Unknown before any sample ran, which is where a refused connection leaves it.
func (d *DBErrors) LogAccess() (access, reason string) {
	if !d.sampled {
		return LogAccessUnknown, reasonSettingsUnread
	}

	if source := d.tail.source; !d.read || source.reason != "" {
		return source.logAccess(), source.logAccessReason()
	}

	return LogAccessDirect, ""
}

// closingFields is the file's account, carried by pg_metadata.txt since db_errors.log
// holds the server's lines and nothing else. Counts appear only beside a log that was
// read, so none stands for "not read"; a limit appears only when it was reached.
func (d *DBErrors) closingFields() []headerField {
	access, reason := d.LogAccess()

	fields := []headerField{{"db_errors_log_access", access}}

	if reason != "" {
		fields = append(fields, headerField{"db_errors_log_access_reason", reason})
	}

	if !d.read {
		return fields
	}

	// The file has no header of its own, so the format its lines are in is said here.
	fields = append(fields,
		headerField{"db_errors_log_format", string(d.format)},
		headerField{"db_errors_matched", strconv.Itoa(d.matched)},
		headerField{"db_errors_dropped", strconv.Itoa(d.dropped)},
	)

	if d.resolvedLate {
		fields = append(fields, headerField{"db_errors_resolved_late", "true"})
	}

	for _, limit := range []struct {
		key   string
		count int64
	}{
		{"db_errors_skipped_bytes", d.skippedBytes},
		{"db_errors_events_truncated", int64(d.eventsTruncated)},
		{"db_errors_rotations", int64(d.rotations)},
		{"db_errors_file_truncations", int64(d.fileTruncations)},
		{"db_errors_carry_dropped", int64(d.carryDropped)},
		{"db_errors_partial_events", int64(d.partialEvents)},
	} {
		if limit.count > 0 {
			fields = append(fields, headerField{limit.key, strconv.FormatInt(limit.count, 10)})
		}
	}

	return fields
}
