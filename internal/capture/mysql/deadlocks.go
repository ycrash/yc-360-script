package mysql

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// MaxScanBytes bounds what one sample reads of the error log; the rest waits for
// the next sample, so a log that grew by gigabytes cannot stall the timeline.
const MaxScanBytes = 16 << 20

// The codes a deadlock report is logged under at log_error_verbosity=3: the
// opening line, and each section's heading.
const (
	codeDeadlockDetected = "MY-012468"
	codeDeadlockSection  = "MY-012469"
)

// Why the deadlock tail reads nothing, as a block's reason=.
const (
	deadlockReasonLogUnread  = "log_settings_unread"
	deadlockReasonUnreadable = "log_unreadable"
)

// Deadlocks is my_deadlocks.txt: every deadlock report innodb_print_all_deadlocks
// writes to the error log during the window, copied with its statements' and rows'
// values replaced. The log is opened at its end on the first sample, so only the
// window's reports are read.
//
// A report has two shapes, and both are read. At log_error_verbosity=3 it opens
// with a [Note] [MY-012468] line and each section is a [Note] [MY-012469] heading;
// at the default verbosity of 2 those notes are filtered out and only InnoDB's raw
// dump remains, opening with a bare "TRANSACTION <id>, ACTIVE" line. Two reports
// logged back to back at verbosity 2 have nothing between them and are read as one.
//
// Where the log cannot be read, the only deadlock the server will show is its most
// recent, in SHOW ENGINE INNODB STATUS's LATEST DETECTED DEADLOCK - which
// my_buffer_pool.txt already captures every sample, so this file names it as the
// fallback rather than run the statement a second time.
type Deadlocks struct {
	Interval time.Duration

	resolved   bool
	path       string
	reason     string
	printAll   string
	file       *os.File
	offset     int64
	pending    []byte
	event      []string
	inEvent    bool
	announced  bool
	rotated    bool
	readErrors int
}

func (d *Deadlocks) Artifact() Artifact {
	return Artifact{
		Name:       "my_deadlocks",
		FileName:   "my_deadlocks.txt",
		Scope:      "cluster",
		Schedule:   dbcore.Every(d.Interval),
		Format:     dbcore.FormatText,
		Connection: ConnectionNormal,
	}
}

func (d *Deadlocks) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	if !d.resolved {
		d.resolve(ctx, q, s)
	}

	if d.file == nil {
		return d.writeReason(w, s, false)
	}

	events, from, to := d.readNew(false)

	return d.writeEvents(w, s, events, from, to, false)
}

// WriteClosing drains what the log gained since the last sample, the report still
// being assembled included, and closes the file.
func (d *Deadlocks) WriteClosing(w io.Writer, s SampleContext) error {
	if d.file == nil {
		return nil
	}

	defer d.file.Close()

	events, from, to := d.readNew(true)

	return d.writeEvents(w, s, events, from, to, true)
}

// resolve finds the log the server writes and opens it at its end.
func (d *Deadlocks) resolve(ctx context.Context, q Querier, s SampleContext) {
	res, err := read(ctx, q, s, block{
		query: showGlobal("VARIABLES", "log_error", "datadir", "innodb_print_all_deadlocks"),
	})
	if err != nil {
		d.reason = deadlockReasonLogUnread

		return
	}

	// Resolved once the settings are read; a log that will not open stays closed for
	// the window rather than be retried each sample.
	d.resolved = true

	values := map[string]string{}
	for _, row := range res.rows {
		values[strings.ToLower(row[0])] = row[1]
	}

	d.printAll = values["innodb_print_all_deadlocks"]

	path, reason := resolveErrorLog(values["log_error"], values["datadir"])
	if path == "" {
		d.reason = reason

		return
	}

	d.path = path

	file, err := os.Open(path)
	if err != nil {
		d.reason = deadlockReasonUnreadable

		return
	}

	info, err := file.Stat()
	if err != nil {
		file.Close()
		d.reason = deadlockReasonUnreadable

		return
	}

	d.file = file
	d.offset = info.Size()
}

// readNew reads what the log gained and returns the reports it completed, with the
// offsets read between. drain completes the report still open.
func (d *Deadlocks) readNew(drain bool) (events []string, from, to int64) {
	from = d.offset
	d.rotated = false

	// The held file first, to its end: after a rotation, that is the old file's
	// last lines, and its last report ends with it. Then the file now at the path,
	// from its start.
	d.readChunk()
	events = d.consumeLines(events)

	if d.followRotation() {
		if event, done := d.finish(); done {
			events = append(events, event)
		}

		d.rotated = true
		d.readChunk()
		events = d.consumeLines(events)
	}

	// InnoDB writes a report in one go, so a read that ended on a line boundary has
	// the whole of it: at the default verbosity nothing else would ever close it.
	if !drain && len(d.pending) == 0 {
		if event, done := d.finish(); done {
			events = append(events, event)
		}
	}

	if drain {
		if len(d.pending) > 0 {
			if event, done := d.consume(string(d.pending)); done {
				events = append(events, event)
			}

			d.pending = nil
		}

		if d.inEvent {
			events = append(events, strings.Join(d.event, "\n"))
			d.event, d.inEvent = nil, false
		}
	}

	return events, from, d.offset
}

// consumeLines reads the whole lines pending - the last may still be being written -
// and appends the reports they complete.
func (d *Deadlocks) consumeLines(events []string) []string {
	for {
		end := bytes.IndexByte(d.pending, '\n')
		if end < 0 {
			return events
		}

		line := string(bytes.TrimRight(d.pending[:end], "\r"))
		d.pending = d.pending[end+1:]

		if event, done := d.consume(line); done {
			events = append(events, event)
		}
	}
}

// readChunk reads the held file from the offset, up to MaxScanBytes.
func (d *Deadlocks) readChunk() {
	if _, err := d.file.Seek(d.offset, io.SeekStart); err != nil {
		d.readErrors++

		return
	}

	chunk, err := io.ReadAll(io.LimitReader(d.file, MaxScanBytes))
	if err != nil && !errors.Is(err, io.EOF) {
		d.readErrors++
	}

	d.offset += int64(len(chunk))
	d.pending = append(d.pending, chunk...)
}

// followRotation reports a log that is no longer the one held: a new file at the
// path, which is opened, or the same file truncated in place. Either is then read
// from its start. A line the old file left unfinished is dropped, so two files'
// halves are never welded into one line.
func (d *Deadlocks) followRotation() bool {
	current, err := os.Stat(d.path)
	if err != nil {
		return false
	}

	held, err := d.file.Stat()
	if err != nil {
		return false
	}

	switch {
	case !os.SameFile(current, held):
		file, err := os.Open(d.path)
		if err != nil {
			return false
		}

		d.file.Close()
		d.file = file

	case held.Size() < d.offset:

	default:
		return false
	}

	d.offset = 0
	d.pending = nil

	return true
}

// consume adds one line to the report being read, and returns a report when the line
// ends one.
func (d *Deadlocks) consume(line string) (string, bool) {
	prefixed := hasLogPrefix(line)
	code, _ := entryCode(line)

	switch {
	case prefixed && code == codeDeadlockDetected:
		event, done := d.finish()
		d.event, d.inEvent = []string{line}, true

		return event, done

	case d.inEvent && (!prefixed || code == codeDeadlockSection):
		d.event = append(d.event, line)

		return "", false

	case !prefixed && strings.HasPrefix(line, "TRANSACTION ") && strings.Contains(line, ", ACTIVE"):
		// Verbosity 2: the dump with no note before it.
		d.event, d.inEvent = []string{line}, true

		return "", false

	case d.inEvent:
		return d.finish()
	}

	return "", false
}

// finish ends the report being read, if one is.
func (d *Deadlocks) finish() (string, bool) {
	if !d.inEvent {
		return "", false
	}

	event := strings.Join(d.event, "\n")
	d.event, d.inEvent = nil, false

	return event, true
}

// writeEvents writes one block of the reports read, values replaced.
func (d *Deadlocks) writeEvents(w io.Writer, s SampleContext, events []string, from, to int64, drain bool) error {
	var body bytes.Buffer

	redacted := 0

	for _, event := range events {
		text, n := redactMonitorText(event)
		redacted += n

		body.WriteString(strings.TrimRight(text, "\n") + "\n")
	}

	fields := d.blockFields(s, drain)

	if !d.announced || d.rotated {
		fields = append(fields, kv("log_path", d.path), kv("log_access", LogAccessDirect))
		d.announced = true
	}

	if d.rotated {
		fields = append(fields, kv("rotated", "true"))
	}

	fields = append(fields,
		kv("from_offset", strconv.FormatInt(from, 10)),
		kv("to_offset", strconv.FormatInt(to, 10)),
		kv("matched", strconv.Itoa(len(events))),
		kv("redacted", strconv.Itoa(redacted)),
	)

	if d.readErrors > 0 {
		fields = append(fields, kv("read_errors", strconv.Itoa(d.readErrors)))
	}

	fields = append(fields, kv("bytes", strconv.Itoa(body.Len())))

	var block bytes.Buffer

	if err := dbcore.WriteBlockHeader(&block, engineName, "my_deadlocks", dbcore.DefaultArtifactVersion, "cluster",
		dbcore.FormatText, fields, s.At); err != nil {
		return err
	}

	block.Write(body.Bytes())

	_, err := w.Write(block.Bytes())

	return err
}

// writeReason writes a sample's block where the log is not read, naming the
// fallback.
func (d *Deadlocks) writeReason(w io.Writer, s SampleContext, drain bool) error {
	fields := append(d.blockFields(s, drain),
		kv("log_path", d.path),
		kv("log_access", LogAccessNone),
		kv("reason", d.reason),
		kv("fallback", "my_buffer_pool.txt:LATEST DETECTED DEADLOCK"),
		kv("bytes", "0"),
	)

	return dbcore.WriteBlockHeader(w, engineName, "my_deadlocks", dbcore.DefaultArtifactVersion, "cluster",
		dbcore.FormatText, fields, s.At)
}

// blockFields opens every block's header: where it was read through, which sample,
// and the setting without which no report is written at all.
func (d *Deadlocks) blockFields(s SampleContext, drain bool) []dbcore.HeaderField {
	fields := location(s)

	if drain {
		fields = append(fields, kv("drain", "true"))
	} else {
		fields = append(fields, kv("sample", strconv.Itoa(s.Index)))
	}

	return append(fields, kv("innodb_print_all_deadlocks", d.printAll))
}
