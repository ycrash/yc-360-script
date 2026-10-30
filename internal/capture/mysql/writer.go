package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// DefaultMaxText bounds one captured text value in runes - statement text, mostly,
// whose server-side copies are cut at 1024 bytes by default. A value past it is cut
// and counted on its block as text_cut=.
const DefaultMaxText = 8192

// Reasons a block was read and found nothing by design, the reason= of a status=OK
// block with no rows.
const (
	// reasonPermissionDenied: the role may not run the statement. The refusal is the
	// same every sample, so it is asked once and remembered: each refusal counts
	// once more against the server's own error statistics, which my_errors.txt
	// reads.
	reasonPermissionDenied = "permission_denied"
)

func kv(key, value string) dbcore.HeaderField { return dbcore.HeaderField{Key: key, Value: value} }

// block is one statement and how its result is written: one block of a sample.
type block struct {
	// source is the block's source=, what was read.
	source string

	// query is the statement, sent as written.
	query string

	// columns are the columns written when the read failed or was not made; a read
	// that succeeded writes the statement's own.
	columns []string

	// statementColumns are the columns holding SQL text, whose values are replaced
	// and counted as redacted=.
	statementColumns []string

	// messageColumns are the columns holding a server's error message, which may
	// quote a value or a statement; replaced and counted the same way.
	messageColumns []string

	// maxRows bounds the rows written; the statement asks for one more, so a cut is
	// known rather than guessed. Zero writes every row.
	maxRows int

	// scope is the artifact's when empty.
	scope string

	// fields are written on the block's header after the envelope's.
	fields []dbcore.HeaderField

	// uncapped reads values whole: a plan has a cap of its own.
	uncapped bool
}

// result is one read.
type result struct {
	columns []string
	rows    [][]string

	// truncated: there were more than maxRows rows.
	truncated bool

	// redacted counts replaced values, textCut values cut at DefaultMaxText.
	redacted int
	textCut  int

	reads dbcore.Span
}

// read runs b's statement under its own deadline and returns every value as text,
// NULL as empty.
func read(ctx context.Context, q Querier, s SampleContext, b block) (result, error) {
	ctx, cancel := statementContext(ctx)
	defer cancel()

	res := result{reads: dbcore.Span{Start: s.Now()}}

	rows, err := q.Query(ctx, b.query)
	if err != nil {
		res.reads.End = s.Now()

		return res, err
	}

	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		res.reads.End = s.Now()

		return res, err
	}

	res.columns = columns

	redactors := make([]func(string) (string, int), len(columns))
	for i, column := range columns {
		redactors[i] = b.redactor(column)
	}

	values := make([]sql.NullString, len(columns))
	dest := make([]any, len(columns))

	for i := range values {
		dest[i] = &values[i]
	}

	for rows.Next() {
		if b.maxRows > 0 && len(res.rows) == b.maxRows {
			res.truncated = true

			break
		}

		if err := rows.Scan(dest...); err != nil {
			res.reads.End = s.Now()

			return res, err
		}

		row := make([]string, len(columns))

		for i, value := range values {
			if b.uncapped {
				row[i] = value.String

				continue
			}

			text, cut, redacted := cell(value.String, redactors[i])

			row[i] = text
			res.redacted += redacted

			if cut {
				res.textCut++
			}
		}

		res.rows = append(res.rows, row)
	}

	if err := rows.Err(); err != nil {
		res.reads.End = s.Now()

		return res, err
	}

	res.reads.End = s.Now()

	return res, nil
}

// redactor is what replaces the values in column, nil for a column that holds none.
func (b block) redactor(column string) func(string) (string, int) {
	named := func(c string) bool { return strings.EqualFold(c, column) }

	switch {
	case slices.ContainsFunc(b.statementColumns, named):
		return redactStatement

	case slices.ContainsFunc(b.messageColumns, named):
		return redactServerMessage
	}

	return nil
}

// cell caps a value, then, with a redactor, replaces its values, so a literal the
// cap split is still found; the cap's "..." goes back on after, where no
// replacement can swallow it.
func cell(value string, redact func(string) (string, int)) (string, bool, int) {
	capped := dbcore.TruncateRunes(value, DefaultMaxText)
	cut := capped != value

	if redact == nil {
		return capped, cut, 0
	}

	if cut {
		capped = strings.TrimSuffix(capped, "...")
	}

	capped, redacted := redact(capped)

	if cut {
		capped += "..."
	}

	return capped, cut, redacted
}

// blockState remembers, across samples, what a collector's blocks were refused.
type blockState struct {
	refused map[string]bool
}

func (st *blockState) isRefused(source string) bool { return st.refused[source] }

func (st *blockState) refuse(source string) {
	if st.refused == nil {
		st.refused = map[string]bool{}
	}

	st.refused[source] = true
}

// blockOutcome is what writing one block found: whether its read failed, and how,
// and how many rows it wrote.
type blockOutcome struct {
	err  error
	rows int

	// written are the rows as written: values capped and replaced.
	written [][]string

	// read: the statement ran and returned, refused neither now nor before.
	read bool
}

// writeBlock reads b and writes it as one sample block of artifact. A refusal for
// want of a privilege is written as reason=permission_denied, and remembered in st
// so the next sample does not ask again; any other failure is the block's
// status=, with error=, and the column header alone.
func writeBlock(ctx context.Context, q Querier, w io.Writer, artifact Artifact, s SampleContext,
	st *blockState, b block,
) (blockOutcome, error) {
	if st != nil && st.isRefused(b.source) {
		return blockOutcome{}, writeReason(w, artifact, s, b, reasonPermissionDenied, dbcore.Span{})
	}

	res, err := read(ctx, q, s, b)

	switch {
	case err != nil && permissionDenied(err):
		if st != nil {
			st.refuse(b.source)
		}

		fields := append(slices.Clone(b.fields), kv("reason", reasonPermissionDenied), kv("error", s.ErrorText(err)))

		return blockOutcome{}, writeColumns(w, artifact, s, b, res.reads, dbcore.StatusOK, fields)

	case err != nil:
		fields := append(slices.Clone(b.fields), kv("error", s.ErrorText(err)))

		return blockOutcome{err: err}, writeColumns(w, artifact, s, b, res.reads, readStatus(err), fields)
	}

	fields := slices.Clone(b.fields)

	if len(b.statementColumns) > 0 || len(b.messageColumns) > 0 {
		fields = append(fields, kv("redacted", strconv.Itoa(res.redacted)))
	}

	if res.textCut > 0 {
		fields = append(fields, kv("text_cut", strconv.Itoa(res.textCut)))
	}

	if err := dbcore.WriteSampleHeader(w, engineName, location(s), artifact, s, dbcore.SampleHeader{
		Source:    b.source,
		Scope:     b.scope,
		Reads:     res.reads,
		Status:    dbcore.StatusOK,
		Rows:      len(res.rows),
		Truncated: res.truncated,
		Fields:    fields,
	}); err != nil {
		return blockOutcome{}, err
	}

	return blockOutcome{rows: len(res.rows), written: res.rows, read: true}, dbcore.WriteRows(w, res.columns, res.rows)
}

// writeReason writes a block that found nothing by design: status=OK, reason=, and
// the column header alone.
func writeReason(w io.Writer, artifact Artifact, s SampleContext, b block, reason string, reads dbcore.Span) error {
	return writeColumns(w, artifact, s, b, reads, dbcore.StatusOK, append(slices.Clone(b.fields), kv("reason", reason)))
}

// writeColumns writes a header and b's declared columns with no rows.
func writeColumns(w io.Writer, artifact Artifact, s SampleContext, b block, reads dbcore.Span, status string,
	fields []dbcore.HeaderField,
) error {
	if err := dbcore.WriteSampleHeader(w, engineName, location(s), artifact, s, dbcore.SampleHeader{
		Source: b.source,
		Scope:  b.scope,
		Reads:  reads,
		Status: status,
		Fields: fields,
	}); err != nil {
		return err
	}

	// A statement whose columns the agent does not declare - SHOW REPLICA STATUS's
	// change between versions - has no header to write when it was not read.
	if len(b.columns) == 0 {
		return nil
	}

	return dbcore.WriteRows(w, b.columns, nil)
}

// writeBlocks writes a sample of several blocks in one write, so a failed write
// cannot leave half a sample. A sample whose every block failed returns the first
// failure and writes nothing, so the window records it as the sample's stub and the
// artifact as partial; a refusal is not a failure.
func writeBlocks(ctx context.Context, q Querier, w io.Writer, artifact Artifact, s SampleContext,
	st *blockState, blocks []block,
) error {
	var (
		buf      bytes.Buffer
		firstErr error
		failed   int
	)

	for _, b := range blocks {
		outcome, err := writeBlock(ctx, q, &buf, artifact, s, st, b)
		if err != nil {
			return err
		}

		if outcome.err != nil {
			failed++

			if firstErr == nil {
				firstErr = outcome.err
			}
		}
	}

	if len(blocks) > 0 && failed == len(blocks) {
		return firstErr
	}

	_, err := w.Write(buf.Bytes())

	return err
}

// limitSQL appends LIMIT n+1 to a statement, so a cut at n is known.
func limitSQL(query string, n int) string {
	return query + "\nLIMIT " + strconv.Itoa(n+1)
}

// queryCollector is an artifact whose every sample is a fixed list of blocks, each
// a statement whose result is written as read.
type queryCollector struct {
	artifact Artifact
	blocks   func(s SampleContext) []block

	state blockState
}

func (c *queryCollector) Artifact() Artifact { return c.artifact }

func (c *queryCollector) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	return writeBlocks(ctx, q, w, c.artifact, s, &c.state, c.blocks(s))
}

// periodicArtifact is a sampled file on one connection. Every MySQL file is about
// the instance: performance_schema and the status counters are server-wide.
func periodicArtifact(name, connection string, interval time.Duration) Artifact {
	return Artifact{
		Name:       name,
		FileName:   name + ".txt",
		Scope:      "cluster",
		Schedule:   dbcore.Periodic(interval),
		Connection: connection,
	}
}

// scannedStrings is what a Scan into read's destinations left, as text.
func scannedStrings(dest []any) []string {
	values := make([]string, len(dest))

	for i, d := range dest {
		if value, ok := d.(*sql.NullString); ok {
			values[i] = value.String
		}
	}

	return values
}

// errNoRows is a single-row read that returned none.
var errNoRows = errors.New("no rows")

// readRow runs a statement expected to return one row, as text.
func readRow(ctx context.Context, q Querier, s SampleContext, query string) ([]string, error) {
	res, err := read(ctx, q, s, block{query: query})
	if err != nil {
		return nil, err
	}

	if len(res.rows) == 0 {
		return nil, errNoRows
	}

	return res.rows[0], nil
}
