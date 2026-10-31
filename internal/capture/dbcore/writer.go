package dbcore

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// DefaultArtifactVersion is v= for an artifact that sets no Version.
const DefaultArtifactVersion = 1

// SampleHeaderVersion is v= for a periodic file whose one break is the two-line sample
// header.
const SampleHeaderVersion = 2

// The two body formats; a receiver dispatches on format=, not filename. FormatText's
// end is given by the block header's bytes= key, never by scanning for the next '#'.
const (
	FormatCSV  = "csv"
	FormatText = "text"
)

// TimestampLayout is every timestamp the agent writes: UTC, milliseconds, a Z.
const TimestampLayout = "2006-01-02T15:04:05.000Z"

// A sample block's status=.
const (
	StatusOK        = "OK"
	StatusError     = "ERROR"
	StatusTimeout   = "TIMEOUT"
	StatusTruncated = "TRUNCATED"
)

// HeaderField is one key=value token of a block header.
type HeaderField struct {
	Key   string
	Value string
}

// Field is one key,value row of a key-value body.
type Field struct {
	Key   string
	Value string
}

// WriteBlockHeader renders one block header line:
//
//	# engine=<engine> source=<source> v=<n> format=<format> scope=<scope> [k=v ...] ts=<ts>
//
// An empty value means "not read"; a missing key means it didn't happen.
func WriteBlockHeader(w io.Writer, engine, source string, version int, scope, format string,
	fields []HeaderField, ts time.Time,
) error {
	line := make([]HeaderField, 0, len(fields)+6)
	line = append(line,
		HeaderField{"engine", engine},
		HeaderField{"source", source},
		HeaderField{"v", strconv.Itoa(version)},
		HeaderField{"format", format},
		HeaderField{"scope", scope},
	)
	line = append(line, fields...)
	line = append(line, HeaderField{"ts", Timestamp(ts)})

	return WriteHeaderLine(w, line)
}

// WriteHeaderLine writes "# k=v k=v ...", each value quoted where it must be.
func WriteHeaderLine(w io.Writer, fields []HeaderField) error {
	tokens := make([]string, len(fields))
	for i, f := range fields {
		tokens[i] = f.Key + "=" + HeaderValue(f.Value)
	}

	_, err := io.WriteString(w, "# "+strings.Join(tokens, " ")+"\n")

	return err
}

// Span is when a block's statements ran, from the first one's start to the last one's
// end. Zero when none ran.
type Span struct {
	Start time.Time
	End   time.Time
}

// SampleHeader is what a sample block's second header line says about the block.
type SampleHeader struct {
	Source string

	// Scope is the artifact's when empty.
	Scope string

	Reads Span

	// Status is a read's status, or StatusTruncated.
	Status string

	Rows      int
	Truncated bool

	// Fields are the block's own keys, written after the envelope's.
	Fields []HeaderField
}

// WriteSampleHeader writes a periodic file's two header lines for one sample block:
//
//	# capture_id=<uuid> target_id=<id> engine=<engine> engine_version=<n> v=<n> format=<f> <location>
//	# sample_id=<n> source=<source> start_ts=<ts> end_ts=<ts> duration_ms=<n> status=<status> rows=<n> truncated=<bool> scope=<scope> [k=v ...] ts=<ts>
//
// The first line is the same on every block of a file; times are empty where no
// statement ran.
func WriteSampleHeader[E any](w io.Writer, engine string, location []HeaderField, artifact Artifact,
	s SampleContext[E], h SampleHeader,
) error {
	capture := make([]HeaderField, 0, len(location)+6)
	capture = append(capture,
		HeaderField{"capture_id", s.CaptureID},
		HeaderField{"target_id", s.TargetID},
		HeaderField{"engine", engine},
		HeaderField{"engine_version", s.EngineVersion},
		HeaderField{"v", strconv.Itoa(artifact.FormatVersion())},
		HeaderField{"format", artifact.BodyFormat()},
	)
	capture = append(capture, location...)

	if err := WriteHeaderLine(w, capture); err != nil {
		return err
	}

	scope := h.Scope
	if scope == "" {
		scope = artifact.Scope
	}

	fields := make([]HeaderField, 0, len(h.Fields)+10)
	fields = append(fields,
		HeaderField{"sample_id", strconv.Itoa(s.Index)},
		HeaderField{"source", h.Source},
		HeaderField{"start_ts", ClockRead(h.Reads.Start)},
		HeaderField{"end_ts", ClockRead(h.Reads.End)},
		HeaderField{"duration_ms", DurationMillis(h.Reads)},
		HeaderField{"status", h.Status},
		HeaderField{"rows", strconv.Itoa(h.Rows)},
		HeaderField{"truncated", strconv.FormatBool(h.Truncated)},
		HeaderField{"scope", scope},
	)
	fields = append(fields, h.Fields...)
	fields = append(fields, HeaderField{"ts", Timestamp(s.At)})

	return WriteHeaderLine(w, fields)
}

// DurationMillis is a span's length in whole milliseconds, empty where nothing ran.
func DurationMillis(reads Span) string {
	if reads.Start.IsZero() {
		return ""
	}

	return strconv.FormatInt(reads.End.Sub(reads.Start).Milliseconds(), 10)
}

// MaxHeaderValue bounds a header value in runes.
const MaxHeaderValue = 200

// HeaderValue quotes anything that would break k=v tokenisation. QuoteToGraphic
// keeps non-ASCII identifiers readable.
func HeaderValue(s string) string {
	s = TruncateRunes(SingleLine(s), MaxHeaderValue)

	if needsHeaderQuoting(s) {
		return strconv.QuoteToGraphic(s)
	}

	return s
}

// needsHeaderQuoting also quotes non-graphic runes, so they show as escapes.
func needsHeaderQuoting(s string) bool {
	for _, r := range s {
		if unicode.IsSpace(r) || r == '"' || !unicode.IsGraphic(r) {
			return true
		}
	}

	return false
}

// TruncateRunes marks the cut without splitting a multi-byte character.
func TruncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}

	return string(runes[:n]) + "..."
}

// WriteRows writes the column header even with no rows: "found nothing", not "not captured".
func WriteRows(w io.Writer, columns []string, rows [][]string) error {
	cw := csv.NewWriter(w)

	if err := cw.Write(SingleLineAll(columns)); err != nil {
		return err
	}

	for _, row := range rows {
		if err := cw.Write(SingleLineAll(row)); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

// SingleLineAll applies SingleLine to every cell.
func SingleLineAll(cells []string) []string {
	flattened := make([]string, len(cells))
	for i, cell := range cells {
		flattened[i] = SingleLine(cell)
	}

	return flattened
}

// WriteKeyValueBody writes the column header and one row per field.
func WriteKeyValueBody(w io.Writer, fields []Field) error {
	return WriteFields(w, append([]Field{{Key: "key", Value: "value"}}, fields...))
}

// WriteFields writes one key,value row per field.
func WriteFields(w io.Writer, fields []Field) error {
	cw := csv.NewWriter(w)

	for _, f := range fields {
		if err := cw.Write([]string{f.Key, SingleLine(f.Value)}); err != nil {
			return err
		}
	}

	cw.Flush()

	return cw.Error()
}

var lineBreaks = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ")

// SingleLine collapses line breaks so one row is one line.
func SingleLine(s string) string {
	return lineBreaks.Replace(s)
}

// Timestamp renders an agent-side clock read in the artifact's timestamp form.
func Timestamp(t time.Time) string {
	return t.UTC().Format(TimestampLayout)
}

// ClockRead renders a clock read; zero is empty, not year one.
func ClockRead(t time.Time) string {
	if t.IsZero() {
		return ""
	}

	return Timestamp(t)
}

// MillisText renders milliseconds; an unmeasured duration is empty.
func MillisText(d time.Duration) string {
	if d <= 0 {
		return ""
	}

	return strconv.FormatFloat(d.Seconds()*1000, 'f', 1, 64)
}

// RedactError renders err on one line with the password replaced.
func RedactError(err error, password string) string {
	if err == nil {
		return ""
	}

	msg := SingleLine(err.Error())

	if password != "" {
		msg = strings.ReplaceAll(msg, password, "<redacted>")
	}

	return msg
}

// DeadlineStatus is TIMEOUT for an expired context, ERROR otherwise.
func DeadlineStatus(err error) string {
	switch {
	case err == nil:
		return StatusOK

	case errors.Is(err, context.DeadlineExceeded):
		return StatusTimeout
	}

	return StatusError
}

// Is the agent on the database's machine? Never a false yes.
const (
	OnDBHostYes     = "yes"
	OnDBHostNo      = "no"
	OnDBHostUnknown = "unknown"
)

// What the host-capture gate did, so a bundle with no host files explains itself.
const (
	HostArtifactsCaptured = "captured"
	HostArtifactsSkipped  = "skipped"
)
