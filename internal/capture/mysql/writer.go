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

// DefaultMaxText caps one text value, in runes; cuts are counted as text_cut=.
const DefaultMaxText = 8192

// reason= values of an empty status=OK block.
const (
	// reasonPermissionDenied is asked once and remembered: each refusal would
	// count in the error statistics my_errors.txt reads.
	reasonPermissionDenied = "permission_denied"

	// reasonPerformanceSchemaOff: instrumented tables are not asked (spec §7).
	reasonPerformanceSchemaOff = "performance_schema_off"
)

func kv(key, value string) dbcore.HeaderField { return dbcore.HeaderField{Key: key, Value: value} }

// block is one statement of a sample and how its result is written.
type block struct {
	source string

	query string

	// columns are written when the read failed or was skipped.
	columns []string

	// statementColumns hold SQL text, redacted and counted as redacted=.
	statementColumns []string

	// messageColumns hold server error messages, redacted the same way.
	messageColumns []string

	// maxRows bounds the rows written (one more is asked, to detect a cut); 0 is all.
	maxRows int

	// counted: the statement selects rowsTotalSelect, written as rows_total= rather
	// than as a column.
	counted bool

	// scope defaults to the artifact's.
	scope string

	fields []dbcore.HeaderField

	// uncapped skips DefaultMaxText: a plan has its own cap.
	uncapped bool

	// instrumented: reads an instrument-filled performance_schema table, empty when
	// performance_schema is off (status and variables tables are not).
	instrumented bool
}

type result struct {
	columns []string
	rows    [][]string

	// truncated: more than maxRows rows, or fewer written than total.
	truncated bool

	total int64

	// redacted counts replaced values, textCut values cut at DefaultMaxText.
	redacted int
	textCut  int

	reads dbcore.Span
}

// read runs b under a statement deadline; values are text, NULL as empty.
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

	totalAt := -1
	if b.counted {
		totalAt = slices.IndexFunc(columns, func(c string) bool { return strings.EqualFold(c, rowsTotalColumn) })
	}

	res.columns = slices.Clone(columns)
	if totalAt >= 0 {
		res.columns = slices.Delete(res.columns, totalAt, totalAt+1)
	}

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

		row := make([]string, 0, len(res.columns))

		for i, value := range values {
			switch {
			case i == totalAt:
				res.total, _ = strconv.ParseInt(value.String, 10, 64)

				continue

			case b.uncapped:
				row = append(row, value.String)

				continue
			}

			text, cut, redacted := cell(value.String, redactors[i])

			row = append(row, text)
			res.redacted += redacted

			if cut {
				res.textCut++
			}
		}

		res.rows = append(res.rows, row)
	}

	if totalAt >= 0 && int64(len(res.rows)) < res.total {
		res.truncated = true
	}

	if err := rows.Err(); err != nil {
		res.reads.End = s.Now()

		return res, err
	}

	res.reads.End = s.Now()

	return res, nil
}

// redactor is column's redaction, or nil.
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

// cell caps then redacts a value, re-appending the cap's "..." after so no
// replacement swallows it.
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

// blockState remembers refused sources across samples.
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

// blockOutcome is what writing one block found.
type blockOutcome struct {
	err  error
	rows int

	// written are the rows as written, capped and redacted.
	written [][]string

	// read: the statement ran and returned.
	read bool
}

// writeBlock reads b and writes one sample block. A privilege refusal is written
// as reason=permission_denied and remembered in st; other failures as status= and
// error=. Instrumented blocks are skipped with performance_schema off.
func writeBlock(ctx context.Context, q Querier, w io.Writer, artifact Artifact, s SampleContext,
	st *blockState, b block,
) (blockOutcome, error) {
	if b.instrumented && s.Engine.PerformanceSchemaOff {
		return blockOutcome{}, writeReason(w, artifact, s, b, reasonPerformanceSchemaOff, dbcore.Span{})
	}

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

	if b.counted {
		fields = append(fields, kv(rowsTotalColumn, strconv.FormatInt(res.total, 10)))
	}

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

// writeReason writes an empty status=OK block with reason=.
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

	// Undeclared columns (e.g. SHOW REPLICA STATUS) have no header to write.
	if len(b.columns) == 0 {
		return nil
	}

	return dbcore.WriteRows(w, b.columns, nil)
}

// writeBlocks writes a sample's blocks in one write. If every block failed it
// writes nothing and returns the first failure; a refusal is not a failure.
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

// rowsTotalSelect is the pre-LIMIT row count: COUNT(*) OVER () runs before LIMIT
// (measured on 8.0.46 and 8.4.13). An empty result totals zero.
const (
	rowsTotalColumn = "rows_total"
	rowsTotalSelect = "COUNT(*) OVER () AS " + rowsTotalColumn
)

// limitSQL appends LIMIT n+1 to a statement, so a cut at n is known.
func limitSQL(query string, n int) string {
	return query + "\nLIMIT " + strconv.Itoa(n+1)
}

// queryCollector samples a fixed list of blocks.
type queryCollector struct {
	artifact Artifact
	blocks   func(s SampleContext) []block

	state blockState
}

func (c *queryCollector) Artifact() Artifact { return c.artifact }

func (c *queryCollector) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	return writeBlocks(ctx, q, w, c.artifact, s, &c.state, c.blocks(s))
}

// periodicArtifact is a sampled, instance-scoped file on one connection.
func periodicArtifact(name, connection string, interval time.Duration) Artifact {
	return Artifact{
		Name:       name,
		FileName:   name + ".txt",
		Scope:      "cluster",
		Schedule:   dbcore.Periodic(interval),
		Connection: connection,
	}
}

// scannedStrings is read's scanned destinations as text.
func scannedStrings(dest []any) []string {
	values := make([]string, len(dest))

	for i, d := range dest {
		if value, ok := d.(*sql.NullString); ok {
			values[i] = value.String
		}
	}

	return values
}

var errNoRows = errors.New("no rows")

// readRow reads a single row as text.
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
