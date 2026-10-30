package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"yc-agent/internal/capture/dbcore"
)

// Plan-capture limits (spec §5), the same on every engine.
const (
	// MaxPlansPerCapture bounds the plans a capture asks for.
	MaxPlansPerCapture = 5

	// MaxPlanBytes bounds one plan; a longer one is cut and written as TRUNCATED.
	MaxPlanBytes = 1 << 20

	// ExplainBudget is the time one plan is allowed. MySQL has no server-side bound
	// on EXPLAIN - max_execution_time covers SELECT only - and the driver closes the
	// connection when a context expires mid-statement, which would end this
	// connection's timeline. So the bound that fires is the server's
	// lock_wait_timeout, which is what an EXPLAIN waits on in practice; the client
	// deadline stays at StatementDeadline; and a plan that took longer than this is
	// still written, as TIMEOUT, with its duration.
	ExplainBudget = 2 * time.Second
)

// Explain modes; config accepts only all, and an omitted key is off.
const explainAll = "all"

// explainableStatement opens a statement EXPLAIN accepts and does not run.
var explainableStatement = regexp.MustCompile(`(?i)^\s*(\(\s*)*(SELECT|WITH|TABLE|INSERT|REPLACE|UPDATE|DELETE)\b`)

// writeStatement opens a statement that writes. The session is read-only
// (transaction_read_only), and the server refuses EXPLAIN of one there with error
// 1792, measured on the matrix; the read-only guard is kept and the plan is not.
var writeStatement = regexp.MustCompile(`(?i)^\s*(INSERT|REPLACE|UPDATE|DELETE)\b`)

// Why a candidate got no plan, as a skipped block's reason=.
const (
	explainReasonOwnStatement = "agent_statement"
	explainReasonNoSample     = "no_sample_text"
	explainReasonNotExplain   = "not_explainable"
	explainReasonCut          = "sample_text_cut"
	explainReasonReadOnly     = "read_only_session"
)

// Explain is my_explain.txt: one estimated plan per digest, the first time the digest
// is seen, at most MaxPlansPerCapture per capture - EXPLAIN FORMAT=JSON of the
// digest's query_sample_text, the one runnable example the server keeps, never
// digest_text, whose ? placeholders do not parse. Never EXPLAIN ANALYZE, which runs
// the statement. The plan's conditions carry the application's values and are
// replaced; its structure, costs and row estimates are kept.
type Explain struct {
	Interval time.Duration

	mode        string
	slowQueries *SlowQueries

	// database is the configured default schema, restored after a plan's USE.
	database string

	seen    map[string]bool
	plans   int
	textCap int
}

// NewExplain reads the candidates slowQueries offers each sample. mode is config's
// explain value; anything but all captures no plans.
func NewExplain(mode string, slowQueries *SlowQueries, interval time.Duration, database string) *Explain {
	return &Explain{
		Interval:    interval,
		mode:        mode,
		slowQueries: slowQueries,
		database:    database,
		seen:        map[string]bool{},
	}
}

func (e *Explain) Artifact() Artifact {
	artifact := periodicArtifact("my_explain", ConnectionNormal, e.Interval)
	artifact.Format = dbcore.FormatText

	// Up to five plans at ExplainBudget each, past the sample's other statements.
	artifact.SampleBudget = DefaultSampleBudget + MaxPlansPerCapture*ExplainBudget

	return artifact
}

func (e *Explain) Sample(ctx context.Context, q Querier, w io.Writer, s SampleContext) error {
	artifact := e.Artifact()

	if e.mode != explainAll {
		return writeTextBlock(w, artifact, s, block{source: "explain"}, dbcore.Span{}, dbcore.StatusOK,
			[]dbcore.HeaderField{kv("reason", "explain_off")}, nil)
	}

	candidates, read := e.slowQueries.Latest()

	var buf bytes.Buffer

	written := 0

	if read == s.Index {
		for _, candidate := range candidates {
			key := candidate.schema + "\x00" + candidate.digest
			if e.seen[key] || e.plans >= MaxPlansPerCapture {
				continue
			}

			e.seen[key] = true

			if err := e.explain(ctx, q, &buf, artifact, s, candidate); err != nil {
				return err
			}

			written++
		}
	}

	// A sample with no new digest still says so, so the file's samples count.
	if written == 0 {
		fields := []dbcore.HeaderField{kv("reason", "no_new_digest"), kv("plans", strconv.Itoa(e.plans))}
		if read != s.Index {
			fields[0] = kv("reason", "slow_queries_unread")
		}

		if err := writeTextBlock(&buf, artifact, s, block{source: "explain"}, dbcore.Span{}, dbcore.StatusOK,
			fields, nil); err != nil {
			return err
		}
	}

	_, err := w.Write(buf.Bytes())

	return err
}

// explain writes one candidate's block: its plan, or why it has none.
func (e *Explain) explain(ctx context.Context, q Querier, w io.Writer, artifact Artifact, s SampleContext,
	c explainCandidate,
) error {
	fields := []dbcore.HeaderField{kv("schema_name", c.schema), kv("digest", c.digest), kv("mode", "ESTIMATED")}
	b := block{source: "explain"}

	if reason := e.skipReason(ctx, q, s, c); reason != "" {
		return writeTextBlock(w, artifact, s, b, dbcore.Span{}, dbcore.StatusOK,
			append(fields, kv("reason", reason)), nil)
	}

	e.plans++

	start := s.Now()

	// EXPLAIN resolves unqualified names against the default schema the statement
	// ran in.
	if c.schema != "" {
		if err := readAll(ctx, q, "USE "+quoteName(c.schema)); err != nil {
			return writeTextBlock(w, artifact, s, b, dbcore.Span{Start: start, End: s.Now()}, readStatus(err),
				append(fields, kv("error", s.ErrorText(err))), nil)
		}

		defer e.restoreSchema(ctx, q)
	}

	res, err := read(ctx, q, s, block{query: "EXPLAIN FORMAT=JSON " + c.text, uncapped: true})
	reads := dbcore.Span{Start: start, End: s.Now()}

	switch {
	case err != nil && permissionDenied(err):
		return writeTextBlock(w, artifact, s, b, reads, dbcore.StatusOK,
			append(fields, kv("reason", reasonPermissionDenied), kv("error", s.ErrorText(err))), nil)

	case err != nil:
		return writeTextBlock(w, artifact, s, b, reads, readStatus(err), append(fields, kv("error", s.ErrorText(err))), nil)
	}

	var plan string
	if len(res.rows) > 0 && len(res.rows[0]) > 0 {
		plan = res.rows[0][0]
	}

	plan, redacted := redactPlan(plan)

	status := dbcore.StatusOK
	if reads.End.Sub(reads.Start) > ExplainBudget {
		status = dbcore.StatusTimeout
	}

	if len(plan) > MaxPlanBytes {
		plan = plan[:MaxPlanBytes]
		status = dbcore.StatusTruncated
	}

	return writeTextBlock(w, artifact, s, b, reads, status,
		append(fields, kv("redacted", strconv.Itoa(redacted))), []byte(plan))
}

// skipReason is why a candidate gets no plan, empty when it gets one.
func (e *Explain) skipReason(ctx context.Context, q Querier, s SampleContext, c explainCandidate) string {
	switch {
	case c.text == "":
		return explainReasonNoSample

	case agentStatement(c.text):
		return explainReasonOwnStatement

	case !explainableStatement.MatchString(c.text):
		return explainReasonNotExplain

	case writeStatement.MatchString(c.text):
		return explainReasonReadOnly
	}

	// A sample cut at performance_schema_max_sql_text_length is not the statement:
	// EXPLAIN would fail on it, or plan a different one.
	if e.textCap == 0 {
		if row, err := readRow(ctx, q, s, "SELECT @@performance_schema_max_sql_text_length"); err == nil {
			e.textCap, _ = strconv.Atoi(row[0])
		}
	}

	if e.textCap > 0 && len(c.text) >= e.textCap {
		return explainReasonCut
	}

	return ""
}

// restoreSchema puts the configured default schema back after a plan's USE. With
// none configured there is no way back to none, and nothing needs one: every agent
// statement names its schema.
func (e *Explain) restoreSchema(ctx context.Context, q Querier) {
	if e.database == "" {
		return
	}

	_ = readAll(ctx, q, "USE "+quoteName(e.database))
}

// quoteName quotes an identifier for MySQL.
func quoteName(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// readAll runs a statement for its effect and drains it.
func readAll(ctx context.Context, q Querier, query string) error {
	ctx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(ctx, query)
	if err != nil {
		return err
	}

	defer rows.Close()

	for rows.Next() {
	}

	return rows.Err()
}

// planConditions are the plan's keys whose values are expressions over the
// statement's values.
var planConditions = regexp.MustCompile(
	`"(attached_condition|index_condition|having_condition|pushed_condition|condition|filter|message|original_condition|resulting_condition)": ("(?:[^"\\]|\\.)*")`)

// redactPlan replaces the values in a JSON plan's conditions; every other key -
// tables, indexes, access types, costs and row estimates - is kept as written.
func redactPlan(plan string) (string, int) {
	redacted := 0

	out := planConditions.ReplaceAllStringFunc(plan, func(match string) string {
		m := planConditions.FindStringSubmatch(match)

		var text string
		if err := json.Unmarshal([]byte(m[2]), &text); err != nil {
			redacted++

			return `"` + m[1] + `": "` + redactedValue + `"`
		}

		text, n := redactStatement(text)
		redacted += n

		var quoted bytes.Buffer

		encoder := json.NewEncoder(&quoted)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(text)

		return `"` + m[1] + `": ` + strings.TrimSuffix(quoted.String(), "\n")
	})

	return out, redacted
}
