package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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

// explainableVerbs open a statement EXPLAIN accepts and does not run.
var explainableVerbs = map[string]bool{
	"SELECT": true, "WITH": true, "TABLE": true, "INSERT": true, "REPLACE": true, "UPDATE": true, "DELETE": true,
}

// writeVerbs open a statement that writes. The session is read-only
// (transaction_read_only), and the server refuses EXPLAIN of one there with error
// 1792, measured on the matrix; the read-only guard is kept and the plan is not.
var writeVerbs = map[string]bool{"INSERT": true, "REPLACE": true, "UPDATE": true, "DELETE": true}

// withVerbs are the statements a WITH can open.
var withVerbs = map[string]bool{"SELECT": true, "TABLE": true, "UPDATE": true, "DELETE": true}

// Why a candidate got no plan, as a skipped block's reason=.
const (
	explainReasonOwnStatement = "agent_statement"
	explainReasonNoSample     = "no_sample_text"
	explainReasonNotExplain   = "not_explainable"
	explainReasonCut          = "sample_text_cut"
	explainReasonReadOnly     = "read_only_session"

	// explainReasonPlanLimit: the capture had already asked for MaxPlansPerCapture
	// plans. The postgres capture's reason for the same limit.
	explainReasonPlanLimit = "plan_limit_reached"

	// explainReasonPlaceholder: a prepared statement's sample keeps its ? markers,
	// which EXPLAIN cannot parse (error 1064, measured on the matrix).
	explainReasonPlaceholder = "placeholder"
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

	// The candidates are the digests, which performance_schema off leaves empty.
	if s.Engine.PerformanceSchemaOff {
		return writeTextBlock(w, artifact, s, block{source: "explain"}, dbcore.Span{}, dbcore.StatusOK,
			[]dbcore.HeaderField{kv("reason", reasonPerformanceSchemaOff)}, nil)
	}

	candidates, read := e.slowQueries.Latest()

	var buf bytes.Buffer

	written := 0

	if read == s.Index {
		for _, candidate := range candidates {
			key := candidate.schema + "\x00" + candidate.digest
			if e.seen[key] {
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

	reason := e.skipReason(ctx, q, s, c)
	if reason == "" && e.plans >= MaxPlansPerCapture {
		reason = explainReasonPlanLimit
	}

	if reason != "" {
		return writeTextBlock(w, artifact, s, b, dbcore.Span{}, dbcore.StatusOK,
			append(fields, kv("reason", reason)), nil)
	}

	e.plans++

	start := s.Now()

	// EXPLAIN resolves unqualified names against the default schema the statement
	// ran in. FOR SCHEMA names it without changing the session's: the digests file
	// every statement under the schema it ran in, and after a USE the agent's own
	// on this connection were filed under the application's.
	statement := "EXPLAIN FORMAT=JSON " + c.text

	switch {
	case c.schema == "":

	case explainsForSchema(s.Engine):
		statement = "EXPLAIN FORMAT=JSON FOR SCHEMA " + quoteName(c.schema) + " " + c.text

	default:
		if err := readAll(ctx, q, "USE "+quoteName(c.schema)); err != nil {
			return writeTextBlock(w, artifact, s, b, dbcore.Span{Start: start, End: s.Now()}, readStatus(err),
				append(fields, kv("error", s.ErrorText(err))), nil)
		}

		defer e.restoreSchema(ctx, q)
	}

	res, err := read(ctx, q, s, block{query: statement, uncapped: true})
	reads := dbcore.Span{Start: start, End: s.Now()}

	switch {
	case err != nil && permissionDenied(err):
		return writeTextBlock(w, artifact, s, b, reads, dbcore.StatusOK,
			append(fields, kv("reason", reasonPermissionDenied), kv("error", s.ErrorText(err))), nil)

	case err != nil:
		return writeTextBlock(w, artifact, s, b, reads, explainStatus(err), append(fields, kv("error", s.ErrorText(err))), nil)
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

// explainStatus is the status of a plan whose read ended in err. A lock wait that
// ran out is the plan's time bound firing - lock_wait_timeout is ExplainBudget, and
// what an EXPLAIN waits on - so it is a plan not obtained within its budget,
// TIMEOUT, as the spec has it, and not an ERROR.
func explainStatus(err error) string {
	if hasErrorNumber(err, errLockWaitTimeout) {
		return dbcore.StatusTimeout
	}

	return readStatus(err)
}

// skipReason is why a candidate gets no plan, empty when it gets one.
func (e *Explain) skipReason(ctx context.Context, q Querier, s SampleContext, c explainCandidate) string {
	switch {
	case c.text == "":
		return explainReasonNoSample

	case agentStatement(c.text):
		return explainReasonOwnStatement

	case !explainableVerbs[statementVerb(c.text)]:
		return explainReasonNotExplain

	case writeVerbs[statementVerb(c.text)]:
		return explainReasonReadOnly

	case hasPlaceholder(c.text):
		return explainReasonPlaceholder
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

// statementVerb is the word that says what a statement does, upper-cased: its first,
// past whitespace, plain comments - an ORM may open every statement with one - and
// opening parentheses. For a WITH it is the statement's past the common table
// expressions, since WITH ... UPDATE writes. Empty when the text opens with neither,
// as an executable comment /*! ... */ does.
func statementVerb(s string) string {
	i := skipSpaceAndComments(s, 0)
	for i < len(s) && s[i] == '(' {
		i = skipSpaceAndComments(s, i+1)
	}

	n := nameLength(s[i:])

	verb := strings.ToUpper(s[i : i+n])
	if verb != "WITH" {
		return verb
	}

	return withVerb(s, i+n)
}

// withVerb is the statement a WITH opens, read from i: the first word at the WITH's
// own level, outside parentheses, literals, quoted names and comments, that opens
// one. WITH when the text has none, as a cut sample may not.
func withVerb(s string, i int) string {
	depth := 0

	for i < len(s) {
		switch c := s[i]; {
		case strings.HasPrefix(s[i:], "/*!"):
			i += 3

		case strings.HasPrefix(s[i:], "/*"):
			i += blockCommentLength(s[i:])

		case c == '#' || lineComment(s[i:]):
			i += lineLength(s[i:])

		case c == '`':
			i += quotedLength(s[i:], '`', false)

		case c == '\'' || c == '"':
			i += quotedLength(s[i:], c, true)

		case literalPrefix(s[i:]) > 0:
			prefix := literalPrefix(s[i:])
			i += prefix + quotedLength(s[i+prefix:], s[i+prefix], true)

		case c == '(':
			depth++
			i++

		case c == ')':
			depth--
			i++

		case isNameByte(c):
			n := nameLength(s[i:])
			if word := strings.ToUpper(s[i : i+n]); depth == 0 && withVerbs[word] {
				return word
			}

			i += n

		default:
			i++
		}
	}

	return "WITH"
}

// explainsForSchema: the server takes EXPLAIN ... FOR SCHEMA. Measured on 8.4.13,
// 9.7.4 and Percona 8.4.11; a syntax error on 8.0.46, and MariaDB has none.
func explainsForSchema(f Facts) bool {
	return f.Variant != VariantMariaDB && f.Version.AtLeast(8, 4, 0)
}

// restoreSchema leaves a plan's schema after its USE, for a server without FOR
// SCHEMA: the configured default schema, or with none information_schema, which
// every account may use. MySQL has no way back to no schema at all, and every
// agent statement names its tables, so any schema serves but the application's.
func (e *Explain) restoreSchema(ctx context.Context, q Querier) {
	schema := e.database
	if schema == "" {
		schema = "information_schema"
	}

	_ = readAll(ctx, q, "USE "+quoteName(schema))
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

// planNameKeys hold names, structure or the optimizer's own numbers, never a value
// from the statement: their strings are kept. Every other string in a plan - a
// condition, an operation ("Filter: (orders.status = 'pending')"), the rewritten
// query 9.x's second JSON format carries whole - has its values replaced, so a key
// a later version adds is replaced until it is listed here.
var planNameKeys = map[string]bool{
	// Tables, indexes and columns.
	"table_name": true, "schema_name": true, "index_name": true, "key": true, "possible_keys": true,
	"used_key_parts": true, "used_columns": true, "key_columns": true, "filter_columns": true,
	"partitions": true, "ref": true, "lookup_references": true,

	// Shape.
	"access_type": true, "index_access_type": true, "join_algorithm": true, "select_type": true,
	"query_type": true, "json_schema_version": true, "key_length": true,

	// Costs and estimates the first JSON format writes as strings.
	"query_cost": true, "read_cost": true, "eval_cost": true, "prefix_cost": true, "sort_cost": true,
	"data_read_per_join": true, "filtered": true,
}

// redactPlan replaces the values in a JSON plan's strings, keeping the strings of
// planNameKeys and the plan's layout byte for byte. JSON numbers - row estimates and
// costs - are the optimizer's, and are kept.
func redactPlan(plan string) (string, int) {
	var out strings.Builder

	out.Grow(len(plan))

	// Each open object or array, with the key its strings fall under: an object's
	// latest key, or for an array the key it is the value of.
	type frame struct {
		object bool
		key    string
	}

	var stack []frame

	redacted := 0

	for i := 0; i < len(plan); {
		switch c := plan[i]; c {
		case '{', '[':
			key := ""
			if len(stack) > 0 {
				key = stack[len(stack)-1].key
			}

			stack = append(stack, frame{object: c == '{', key: key})
			out.WriteByte(c)
			i++

		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}

			out.WriteByte(c)
			i++

		case '"':
			end := jsonStringEnd(plan, i)
			token := plan[i:end]
			i = end

			top := len(stack) - 1
			if top >= 0 && stack[top].object && followedByColon(plan, i) {
				var key string
				_ = json.Unmarshal([]byte(token), &key)
				stack[top].key = key

				out.WriteString(token)

				continue
			}

			if top >= 0 && planNameKeys[stack[top].key] {
				out.WriteString(token)

				continue
			}

			var text string
			if err := json.Unmarshal([]byte(token), &text); err != nil {
				out.WriteString(`"` + redactedValue + `"`)
				redacted++

				continue
			}

			replaced, n := redactStatement(text)
			redacted += n

			if n == 0 {
				out.WriteString(token)

				continue
			}

			var quoted bytes.Buffer

			encoder := json.NewEncoder(&quoted)
			encoder.SetEscapeHTML(false)
			_ = encoder.Encode(replaced)

			out.WriteString(strings.TrimSuffix(quoted.String(), "\n"))

		default:
			out.WriteByte(c)
			i++
		}
	}

	return out.String(), redacted
}

// jsonStringEnd is the index just past the JSON string opening at i, or the end.
func jsonStringEnd(s string, i int) int {
	for j := i + 1; j < len(s); j++ {
		switch s[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}

	return len(s)
}

// followedByColon reports a string that is an object's key.
func followedByColon(s string, i int) bool {
	for ; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		case ':':
			return true
		}

		return false
	}

	return false
}
