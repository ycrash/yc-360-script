package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Explain modes, mirroring config.Postgres.Explain. This package does not import
// internal/config, so a test in internal/capture pins the spellings equal. There is no
// constant for off: the key's absence is the off switch.
const (
	// ExplainModeLogged captures only plans the server itself logged; nothing is
	// submitted back, so it needs no grant beyond the connection.
	ExplainModeLogged = "logged"

	// ExplainModeAll adds the estimated plan modes, which submit EXPLAIN statements
	// built from captured query text.
	ExplainModeAll = "all"

	// explainModeOff is what an omitted key reports as in pg_metadata.txt; it is not an
	// accepted input, since presence is the switch.
	explainModeOff = "off"

	// explainLiteralsVerbatim states the literals policy as a fact in the bundle.
	explainLiteralsVerbatim = "verbatim"
)

// explainModeText renders the configured mode; anything unrecognised is off.
func explainModeText(mode string) string {
	if mode == ExplainModeLogged || mode == ExplainModeAll {
		return mode
	}

	return explainModeOff
}

const (
	// DefaultMaxExplains bounds how many ranked candidates are submitted. Not
	// configurable: a tunable N produces plan counts nobody can reproduce from a bundle.
	DefaultMaxExplains = 10

	// ExplainTimeout bounds one candidate, applied server-side with
	// SET statement_timeout / RESET and never as a client context: pgx closes the
	// connection when a context expires and this window never reconnects. Server-side,
	// a timeout is an ordinary error= and the next candidate proceeds.
	ExplainTimeout = 3 * time.Second

	// ExplainBudget is the aggregate across candidates, deliberately under
	// DefaultMaxExplains x ExplainTimeout so candidates_skipped_budget= can fire.
	ExplainBudget = 20 * time.Second

	// DefaultMaxUnattachedPlans bounds logged plans carrying no query identifier - on
	// PostgreSQL 14/15, and under log_verbose=off anywhere, that is all of them.
	DefaultMaxUnattachedPlans = 20

	// MaxRetainedPlanBytes bounds retained plan bytes across the window, oldest-first.
	MaxRetainedPlanBytes = 4 << 20

	// MaxPlanBytes bounds one submitted plan; nothing else does, and VERBOSE over a
	// heavily partitioned relation returns tens of megabytes.
	MaxPlanBytes = 1 << 20

	// MaxActivitySessions bounds the closing activity read; same view as Sessions, so
	// the same cap. Without it a cluster at max_connections returns every backend's
	// query text into one slice.
	MaxActivitySessions = DefaultMaxSessions
)

// Which view a candidate came from, written as candidate_source=: source= is the
// mandatory first header token naming the artifact.
const (
	candidateSourceStatements = "statements"
	candidateSourceActivity   = "activity"
)

// How the selection was ordered, written once in the summary block.
const (
	rankingStatementsDelta  = "statements_delta"
	rankingActivityFallback = "activity_fallback"
)

// The plan mode that produced a block's body, written as mode=; none where nothing
// was submitted.
const (
	planModeNone             = "none"
	planModeLogged           = "LOGGED"
	planModeEstimatedLiteral = "ESTIMATED_LITERAL"
	planModeEstimatedGeneric = "ESTIMATED_GENERIC"
)

// The ways this artifact can hold no plan without a read having failed.
const (
	// reasonExplainDisabled: the postgres.explain key was omitted - the default.
	reasonExplainDisabled = "explain_disabled"

	// reasonNoCandidates: nothing rankable and no activity row to fall back to - an
	// idle database, not a failure.
	reasonNoCandidates = "no_candidates"

	// reasonQueryTruncated: the only text was cut mid-token and unmarked at
	// track_activity_query_size, with no normalized form to fall back to.
	reasonQueryTruncated = "query_truncated"

	// reasonGenericPlanUnsupported: only parameterized text, and the server is below 16.
	reasonGenericPlanUnsupported = "generic_plan_unsupported"

	// reasonNoLoggedPlan: mode logged, and the store held no entry for this candidate.
	reasonNoLoggedPlan = "no_logged_plan"

	// reasonBudgetSpent: the aggregate ran out before this candidate's turn.
	reasonBudgetSpent = "budget_spent"

	// reasonTextTruncated: the text was cut by the agent's own cap, not the server's.
	// Both query-text reads ask for one rune past DefaultMaxQueryText so the cut is
	// detectable; the prefix ends mid-token and is not submittable.
	reasonTextTruncated = "text_truncated"

	// reasonMultiStatement: the text carries more than one command. The simple protocol
	// runs every statement in one, so EXPLAIN <batch> would execute the customer's
	// statements after the first.
	reasonMultiStatement = "multi_statement"

	// reasonNoQueryIdentifier: a stored plan the log gave no identifier for, so it
	// cannot be joined to a candidate. Below PostgreSQL 16 that is every plan. Losing
	// the join is better than inventing one from query text.
	reasonNoQueryIdentifier = "no_query_identifier"

	// reasonNoRankedReport: an identified plan emitted by the closing pass, where the
	// closing sample never ran and there were no candidates to attach it to.
	reasonNoRankedReport = "no_ranked_report"
)

// planDuration reads the duration the entry declares; the slowest execution is the
// pathological plan, and the one worth keeping.
var planDuration = regexp.MustCompile(`duration:\s+([0-9.]+)\s+ms`)

// setExplainTimeoutSQL is formatted from the constant so the literal cannot drift.
var setExplainTimeoutSQL = fmt.Sprintf("SET statement_timeout TO '%dms'",
	ExplainTimeout.Milliseconds())

const resetExplainTimeoutSQL = `RESET statement_timeout`

// activitySQL is one read at the closing tick, carrying five facts that cost no extra
// statement: search_path, the capture role's OID (self-exclusion), auto_explain
// visibility, track_activity_query_size (the truncation gate) and the encoding's widest
// character (which widens that gate). They come from pg_settings and pg_roles, never
// current_setting() or ::regrole, both of which fail the whole statement - on the "1kB"
// display form and on a role name needing quotes. A one-row CTE LEFT JOINs the view, so
// they arrive even when no session matches. left() applies the agent's cap one rune past
// it, so agentTruncated can tell a cut prefix from a statement that ended there, and
// ORDER BY is activityElapsed's own expression, so a binding LIMIT sheds the
// shortest-running work.
const activitySQL = `WITH r AS (
    SELECT current_setting('search_path') AS search_path,
           (SELECT oid::text FROM pg_catalog.pg_roles
             WHERE rolname = current_user) AS self_oid,
           EXISTS (SELECT 1 FROM pg_catalog.pg_settings
                    WHERE name = 'auto_explain.log_min_duration') AS auto_explain_visible,
           (SELECT setting::int FROM pg_catalog.pg_settings
             WHERE name = 'track_activity_query_size') AS activity_query_size,
           pg_catalog.pg_encoding_max_length(
               pg_catalog.pg_char_to_encoding(current_setting('server_encoding')))::int
               AS max_char_bytes
)
SELECT r.search_path,
       r.self_oid,
       r.auto_explain_visible,
       r.activity_query_size,
       r.max_char_bytes,
       a.pid,
       a.query_id,
       a.datid::text,
       a.usesysid::text,
       a.state,
       left(a.query, $2),
       octet_length(a.query),
       a.query_start,
       a.state_change,
       EXTRACT(EPOCH FROM (now() - a.query_start))::float8,
       EXTRACT(EPOCH FROM (a.state_change - a.query_start))::float8
  FROM r
  LEFT JOIN pg_catalog.pg_stat_activity a
    ON a.datname = current_database()
   AND a.backend_type = 'client backend'
   AND a.pid <> pg_backend_pid()
 ORDER BY CASE WHEN a.state = 'active'
               THEN EXTRACT(EPOCH FROM (now() - a.query_start))
               ELSE EXTRACT(EPOCH FROM (a.state_change - a.query_start))
          END DESC NULLS LAST
 LIMIT $1`

// activityParameter marks text an extended-protocol execution left parameterized, which
// plain EXPLAIN refuses with "there is no parameter $1". No parser is needed because the
// scan errs one way only: a literal '$1' in a string downgrades one candidate to the
// generic mode, while a real parameter can never hide.
var activityParameter = regexp.MustCompile(`\$[0-9]+`)

// explainableKeywords is an allowlist, not a utility blocklist: a blocklist misses CALL,
// COPY, DO, SET, PREPARE, FETCH, EXECUTE and EXPLAIN itself, each of which puts an error
// in the customer's log. Fails closed; losing CREATE TABLE AS is the price.
var explainableKeywords = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "MERGE", "WITH", "VALUES", "TABLE",
}

// Explain reports query plans for the window's most expensive statements. It is the
// only artifact that submits SQL rather than reading it, and only under mode all.
type Explain struct {
	mode string

	// sq supplies the ranking's two endpoint reads; Explain never re-runs them.
	sq *SlowQueries

	tail logTail

	// now is the budget's clock, injectable so a test can spend it.
	now func() time.Time

	// reported gates WriteClosing: the closing sample already emitted everything.
	reported bool

	// tailOpened records that the opening sample ran; false is the connect-failure path,
	// where nothing was attempted.
	tailOpened bool
}

// NewExplain panics on a nil SlowQueries or an unrecognised mode: both are wiring bugs
// at a call site config already validated, and treating a bad mode as off would write
// reason=explain_disabled about a run that asked for plans.
func NewExplain(mode string, sq *SlowQueries) *Explain {
	if sq == nil {
		panic("postgres: NewExplain requires the SlowQueries collector whose endpoints it ranks")
	}

	if mode != "" && mode != ExplainModeLogged && mode != ExplainModeAll {
		panic("postgres: NewExplain got an unvalidated explain mode: " + mode)
	}

	return &Explain{
		mode: mode,
		sq:   sq,
		tail: newLogTail("pg_explain", explainMatch),
		now:  time.Now,
	}
}

func (e *Explain) Artifact() Artifact {
	return Artifact{
		Name:     "pg_explain",
		FileName: "pg_explain.txt",

		// database, not cluster: plans are obtainable only for the connected database,
		// even though the ranking source is cluster-wide.
		Scope: "database",

		Schedule: StartEnd(),
		Format:   formatText,

		SampleBudget: e.sampleBudget(),
	}
}

// sampleBudget is mode-dependent: a disabled feature must not buy every customer the
// enabled one's worst case.
func (e *Explain) sampleBudget() time.Duration {
	if !e.enabled() {
		// Declared explicitly because zero maps to the 20s default.
		return StatementTimeout
	}

	// The aggregate, plus the one candidate that can start just under it and still run
	// its full server-side timeout, plus the one pg_stat_activity read.
	return ExplainBudget + ExplainTimeout + StatementTimeout
}

func (e *Explain) enabled() bool { return e.mode == ExplainModeLogged || e.mode == ExplainModeAll }

// submits reports whether this run sends statements to the server; only mode all does.
func (e *Explain) submits() bool { return e.mode == ExplainModeAll }

// Sample arms the log tail as the window opens and writes the report as it closes; the
// two are separate conditions, since a one-tick window does both in one sample.
func (e *Explain) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	if s.Index == 1 && e.enabled() {
		// Registration order puts this after the other collectors' t0 statements,
		// so the tail opens past the agent's own first burst of plans.
		e.tailOpened = true

		e.tail.openAtEnd(ctx, q, s)
	}

	if s.Index < s.Total {
		return nil
	}

	return e.report(ctx, q, w, s)
}

// report writes the closing sample's blocks. Disabled is one block and no summary: a
// summary of zeroes would read as an empty database rather than a feature left off.
func (e *Explain) report(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	if !e.enabled() {
		return e.writeBlock(w, s, []headerField{{"reason", reasonExplainDisabled}}, nil)
	}

	e.reported = true

	events, read := e.tail.readEvents(ctx, q, time.Time{})
	e.tail.closeFile()

	store := newPlanStore(events)

	activity, facts, activityErr := readActivity(ctx, q)

	selection := e.selectCandidates(s, activity, facts)

	// Attachment runs before submission: the LOGGED mode is the plan for the execution
	// that actually happened, so a candidate it claims is never also submitted.
	store.attach(selection.candidates)

	if e.submits() {
		e.submitAll(ctx, q, s, selection.candidates, &selection.counters)
	}

	for _, c := range selection.candidates {
		if err := e.writeCandidate(w, s, c, facts); err != nil {
			return err
		}
	}

	// Identifier-less plans follow the ranked blocks. An identified plan no candidate
	// claimed stays retained and unwritten, showing as plans_harvested= minus
	// plans_written=.
	for _, plan := range store.unattached() {
		if err := e.writePlan(w, s, plan); err != nil {
			return err
		}

		store.written++
	}

	return e.writeSummary(w, s, selection, facts, store, read, s.errorText(activityErr))
}

// WriteClosing acts only where the closing sample did not, so a cancelled or
// deadline-exceeded window still ships what the tail collected. No connection here, so it
// never re-resolves and never follows a rotation.
func (e *Explain) WriteClosing(w io.Writer, s SampleContext) error {
	if e.reported || !e.enabled() || !e.tailOpened {
		// An unopened tail is the connect-failure path: a summary would report plans_harvested=0
		// about a log nobody opened, and captureMode() over a zero logSource would claim
		// the agent ran on the database host. An opened tail that never resolved still
		// reports - log_reason= and capture_mode= were actually determined.
		return nil
	}

	events, read := e.tail.readEvents(context.Background(), nil, e.now().Add(LogDrainBudget))
	e.tail.closeFile()

	store := newPlanStore(events)

	for _, plan := range store.all() {
		if err := e.writePlan(w, s, plan); err != nil {
			return err
		}

		store.written++
	}

	return e.writeSummary(w, s, explainSelection{ranking: rankingStatementsDelta},
		activityFacts{}, store, read, "")
}

// --- the logged-plan store --------------------------------------------------

// loggedPlan is one auto_explain entry kept for attachment. body is the server's
// bytes verbatim, including the log_line_prefix and the entry's own duration line.
type loggedPlan struct {
	queryID  string
	duration float64
	body     []byte
}

// planStore bounds what the window's plans cost: the slowest entry per query
// identifier, plus the newest DefaultMaxUnattachedPlans identifier-less ones, all under
// MaxRetainedPlanBytes with oldest-first eviction. Under log_min_duration=0, two minutes
// of a busy server is thousands of plans.
type planStore struct {
	// retained is oldest-first, which is also the eviction order.
	retained []*loggedPlan

	byID map[string]*loggedPlan

	// seen counts identified entries, so a block can say how many executions it is the
	// slowest of. Survives eviction.
	seen map[string]int

	// claimed marks identifiers a candidate took, so they are not written twice.
	claimed map[string]bool

	total   int
	dropped int
	written int

	// ambiguous counts candidates naming an already-claimed identifier; the log carries
	// no role or database, so which of them ran the logged execution is unknowable.
	ambiguous int

	bytes int
}

func newPlanStore(events [][]byte) *planStore {
	store := &planStore{
		byID:    map[string]*loggedPlan{},
		seen:    map[string]int{},
		claimed: map[string]bool{},
	}

	for _, event := range events {
		store.add(event)
	}

	return store
}

func (p *planStore) add(event []byte) {
	p.total++

	plan := parsePlanEvent(event)

	if plan.queryID != "" {
		p.seen[plan.queryID]++

		existing, ok := p.byID[plan.queryID]
		if ok {
			if plan.duration <= existing.duration {
				p.dropped++

				return
			}

			p.remove(existing)
			p.dropped++
		}

		p.byID[plan.queryID] = plan
	}

	p.retained = append(p.retained, plan)
	p.bytes += len(plan.body)

	p.trimUnattached()
	p.trimToBudget()
}

// trimUnattached keeps the newest DefaultMaxUnattachedPlans identifier-less entries.
func (p *planStore) trimUnattached() {
	for p.countUnattached() > DefaultMaxUnattachedPlans {
		for _, plan := range p.retained {
			if plan.queryID == "" {
				p.remove(plan)
				p.dropped++

				break
			}
		}
	}
}

func (p *planStore) trimToBudget() {
	for p.bytes > MaxRetainedPlanBytes && len(p.retained) > 0 {
		p.remove(p.retained[0])
		p.dropped++
	}
}

func (p *planStore) countUnattached() int {
	count := 0

	for _, plan := range p.retained {
		if plan.queryID == "" {
			count++
		}
	}

	return count
}

func (p *planStore) remove(plan *loggedPlan) {
	for i, held := range p.retained {
		if held == plan {
			p.retained = append(p.retained[:i], p.retained[i+1:]...)
			p.bytes -= len(plan.body)

			break
		}
	}

	if plan.queryID != "" && p.byID[plan.queryID] == plan {
		delete(p.byID, plan.queryID)
	}
}

// attach promotes every candidate the store holds a plan for, joining on the query
// identifier and nothing else: a wrong attachment is worse than a missing one.
func (p *planStore) attach(candidates []*explainCandidate) {
	for _, c := range candidates {
		if c.queryid == nil {
			continue
		}

		id := strconv.FormatInt(*c.queryid, 10)

		plan, ok := p.byID[id]
		if !ok {
			continue
		}

		if p.claimed[id] {
			// The same queryid under a different role. The entry names an identifier and
			// nothing else, so neither candidate can be shown to be the logged execution,
			// and attaching to both would write one plan under two headers. The
			// highest-ranked keeps it; the rest take their ordinary path.
			p.ambiguous++

			continue
		}

		p.claimed[id] = true
		p.written++

		c.mode = planModeLogged
		c.plan = plan.body
		c.planQueryID = plan.queryID
		c.plansSeen = p.seen[id]

		// Set here on all three formats: the engine would say sqlstate, but 00000 is
		// every LOG line and the message predicate is what actually selected this.
		c.matchedBy = matchedByMessage

		// Stated, not computed: byID is keyed by the plan's identifier, so the join that
		// produced this attachment already is the equality.
		c.queryIDMatch = "true"
	}
}

// unattached is what the ranked report writes after its candidates: the entries the log
// gave no identifier for. An identified plan no candidate claimed stays unwritten.
func (p *planStore) unattached() []*loggedPlan {
	var plans []*loggedPlan

	for _, plan := range p.retained {
		if plan.queryID == "" {
			plans = append(plans, plan)
		}
	}

	return plans
}

// all is the closing pass's set: no ranked report to attach to, so everything is written.
func (p *planStore) all() []*loggedPlan { return p.retained }

// parsePlanEvent reads the two values the entry declares about itself. A non-text
// log_format yields neither, so the body is stored verbatim and never attached.
func parsePlanEvent(event []byte) *loggedPlan {
	plan := &loggedPlan{body: event, queryID: planQueryIdentifier(event)}

	if match := planDuration.FindSubmatch(event); match != nil {
		plan.duration, _ = strconv.ParseFloat(string(match[1]), 64)
	}

	return plan
}

// --- selection ---------------------------------------------------------------

// explainCounters is the summary block's account of a selection that is otherwise
// invisible: which rows were dropped, and for what.
type explainCounters struct {
	considered int

	excludedOtherDatabase int
	excludedMasked        int
	excludedUtility       int
	excludedNotToplevel   int
	excludedSelf          int

	invalid    int
	restarted  int
	unrankable int
	noExecTime int

	written       int
	skippedBudget int
}

// fields omits zero-valued counters: a key present at zero is a measurement, and most
// of these never fire.
func (c explainCounters) fields() []headerField {
	counted := []struct {
		key   string
		value int
	}{
		{"candidates_considered", c.considered},
		{"excluded_other_database", c.excludedOtherDatabase},
		{"excluded_masked", c.excludedMasked},
		{"excluded_utility", c.excludedUtility},
		{"excluded_not_toplevel", c.excludedNotToplevel},
		{"excluded_self", c.excludedSelf},
		{"candidates_invalid", c.invalid},
		{"candidates_restarted", c.restarted},
		{"candidates_unrankable", c.unrankable},
		{"excluded_no_exec_time", c.noExecTime},
		{"candidates_written", c.written},
		{"candidates_skipped_budget", c.skippedBudget},
	}

	fields := make([]headerField, 0, len(counted))

	for _, f := range counted {
		if f.value > 0 {
			fields = append(fields, headerField{f.key, strconv.Itoa(f.value)})
		}
	}

	return fields
}

// explainCandidate is one selected statement and everything the block about it says.
type explainCandidate struct {
	rank   int
	source string

	queryid *int64

	// userid and dbid complete the identity: queryid alone is not unique across roles or
	// databases, and the activity join needs all three.
	userid string
	dbid   string

	// pid and queryStart identify a fallback candidate, which has no queryid the
	// statements view would recognise.
	pid        *int32
	queryStart *time.Time

	// running: the clock has not stopped, so elapsed time is a lower bound and not
	// comparable with a completed duration. Recorded once, not per sort comparison.
	running bool

	// sortKey is the delta or the elapsed time. Never written: it is an in-memory sort
	// key, not RCA judgement.
	sortKey float64

	// literalText is submittable as-is; genericText carries $n placeholders and needs
	// GENERIC_PLAN. Either may be empty.
	literalText string
	genericText string

	// textReason says which cut left this candidate with nothing to submit.
	textReason string

	mode   string
	reason string
	err    string

	planQueryID  string
	queryIDMatch string

	// matchedBy and plansSeen ride the LOGGED mode only.
	matchedBy string
	plansSeen int

	plan []byte

	// planTruncated: the plan came back past MaxPlanBytes and the body is a prefix.
	planTruncated bool
}

type explainSelection struct {
	ranking    string
	candidates []*explainCandidate
	counters   explainCounters
}

// selectCandidates ranks, filters and cuts. The activity fallback runs when the
// statements delta cannot: extension unread, stats reset, or nothing survived the rules.
func (e *Explain) selectCandidates(
	s SampleContext, activity []activityRow, facts activityFacts,
) explainSelection {
	selection := explainSelection{ranking: rankingStatementsDelta}

	ranked := e.rankStatements(s, facts, &selection.counters)

	if ranked == nil {
		selection.ranking = rankingActivityFallback

		// The statements-side counters are the record of why the fallback ran, so they
		// stay. considered becomes the activity count only where that ranking never ran.
		considered := selection.counters.considered
		selection.counters.considered = 0

		ranked = rankActivity(activity, &selection.counters)

		if considered > 0 {
			selection.counters.considered = considered
		}
	}

	if len(ranked) > DefaultMaxExplains {
		ranked = ranked[:DefaultMaxExplains]
	}

	for i, c := range ranked {
		c.rank = i + 1
		c.mode = planModeNone
	}

	selection.counters.written = len(ranked)
	selection.candidates = ranked

	attachText(ranked, activity, facts)

	return selection
}

// rankStatements returns nil - the fallback's signal - both when the ranking cannot run
// and when nothing qualified; an empty slice would suppress the fallback.
func (e *Explain) rankStatements(
	s SampleContext, facts activityFacts, counters *explainCounters,
) []*explainCandidate {
	endpoints := e.sq.rankingEndpoints()

	if !endpoints.endRead || statsWereReset(endpoints) {
		return nil
	}

	counters.considered = len(endpoints.end)

	var ranked []*explainCandidate

	for _, row := range endpoints.end {
		if !eligible(row, s, facts, counters) {
			continue
		}

		key, _ := statementRowKey(row)

		sortKey, ok := rankKey(endpoints, key, row, counters)
		if !ok {
			continue
		}

		if sortKey <= 0 {
			// Nothing accrued between the endpoints: the statement never ran inside the
			// window, so submitting an EXPLAIN for it would spend the budget on nothing.
			counters.noExecTime++

			continue
		}

		ranked = append(ranked, &explainCandidate{
			source:      candidateSourceStatements,
			queryid:     row.queryid,
			userid:      key.userid,
			dbid:        key.dbid,
			sortKey:     sortKey,
			genericText: statementQueryText(row.query),
			textReason:  statementTextReason(row.query),
		})
	}

	if len(ranked) == 0 {
		return nil
	}

	sortByKeyDescending(ranked)

	return ranked
}

// statsWereReset compares the two info readings that bracket the window. A reset between
// them means every delta in the file spans it and none is trustworthy.
func statsWereReset(endpoints statementEndpoints) bool {
	start, end := endpoints.startInfo, endpoints.endInfo
	if start == nil || end == nil {
		return false
	}

	switch {
	case start.statsReset == nil && end.statsReset == nil:
		return false

	case start.statsReset == nil || end.statsReset == nil:
		return true
	}

	return !start.statsReset.Equal(*end.statsReset)
}

// eligible applies the four unexplainable classes plus self-exclusion before the top-N
// cut: a global top ten none of which is explainable serves nobody.
func eligible(row statementRow, s SampleContext, facts activityFacts, counters *explainCounters) bool {
	if row.queryid == nil {
		counters.excludedMasked++

		return false
	}

	if s.DBID != "" && text(row.dbid) != s.DBID {
		counters.excludedOtherDatabase++

		return false
	}

	if facts.selfOID != "" && text(row.userid) == facts.selfOID {
		counters.excludedSelf++

		return false
	}

	// Explicit false only. NULL is extension 1.8, which has no such column, and is
	// not a claim that the statement was nested.
	if row.toplevel != nil && !*row.toplevel {
		counters.excludedNotToplevel++

		return false
	}

	if !explainable(text(row.query)) {
		counters.excludedUtility++

		return false
	}

	return true
}

// rankKey applies the three validity rules and reports whether the row ranks at all.
func rankKey(
	endpoints statementEndpoints, key statementKey, row statementRow, counters *explainCounters,
) (float64, bool) {
	end := floatValue(row.totalExecTime)

	start, both := endpoints.start[key]
	if !both {
		// Rule 3. Under a truncated or unread start endpoint, absence proves nothing:
		// zero-baselining would rank a long-lived query by its lifetime total.
		if !endpoints.startRead || endpoints.startTruncated || endpoints.endTruncated {
			counters.unrankable++

			return 0, false
		}

		return end, true
	}

	// Rule 2. A row whose stats_since moved was evicted and re-created, or
	// targeted-reset; its genuine in-window accrual is the end value.
	if restarted(start.statsSince, row.statsSince) {
		counters.restarted++

		return end, true
	}

	delta := end - floatValue(start.totalExecTime)
	if delta < 0 {
		// The extension is too old to prove a restart, so the cause is unknowable.
		counters.invalid++

		return 0, false
	}

	return delta, true
}

// restarted needs both readings: below extension 1.11 stats_since does not exist, both
// are NULL, and a negative delta stays unexplained rather than being excused.
func restarted(start, end *time.Time) bool {
	if start == nil || end == nil {
		return false
	}

	return !start.Equal(*end)
}

// rankActivity is the fallback: the longest-running statements in the same closing read,
// state-aware because now() - query_start is wrong for an idle session.
func rankActivity(activity []activityRow, counters *explainCounters) []*explainCandidate {
	var ranked []*explainCandidate

	for _, row := range activity {
		if row.pid == nil {
			continue
		}

		counters.considered++

		if !explainable(text(row.query)) {
			counters.excludedUtility++

			continue
		}

		elapsed, ok := activityElapsed(row)
		if !ok {
			counters.unrankable++

			continue
		}

		ranked = append(ranked, &explainCandidate{
			source:     candidateSourceActivity,
			queryid:    row.queryID,
			userid:     text(row.usesysid),
			dbid:       text(row.datid),
			pid:        row.pid,
			queryStart: row.queryStart,
			running:    activityStateRunning(text(row.state)),
			sortKey:    elapsed,
			textReason: reasonQueryTruncated,
		})
	}

	// Running rows first as a class - their elapsed time is a lower bound, not comparable
	// with a completed duration - then by elapsed time within each class. The flag is
	// recorded at construction rather than rescanned inside every comparison.
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].running != ranked[j].running {
			return ranked[i].running
		}

		return ranked[i].sortKey > ranked[j].sortKey
	})

	return ranked
}

// activityElapsed is now() - query_start while running, and state_change - query_start
// once finished - which is every state but active, idle in transaction included.
func activityElapsed(row activityRow) (float64, bool) {
	if activityStateRunning(text(row.state)) {
		if row.runningFor == nil {
			return 0, false
		}

		return *row.runningFor, true
	}

	if row.ranFor == nil {
		return 0, false
	}

	return *row.ranFor, true
}

// activityStateRunning is state = 'active' and nothing else: in every other state
// query_start describes the last statement, so now() - query_start counts the session's
// idle time too and would rank a 2ms query ahead of every active statement.
func activityStateRunning(state string) bool {
	return state == "active"
}

func sortByKeyDescending(ranked []*explainCandidate) {
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].sortKey > ranked[j].sortKey })
}

// attachText finds each candidate's submittable text and applies the two truncation
// gates.
func attachText(candidates []*explainCandidate, activity []activityRow, facts activityFacts) {
	for _, c := range candidates {
		row, ok := matchActivity(c, activity)
		if !ok {
			continue
		}

		query := text(row.query)
		if query == "" {
			continue
		}

		switch {
		case truncatedActivityText(row, facts):
			// Cut mid-token at track_activity_query_size: submitting it would be a
			// syntax error in the customer's own log.
			c.textReason = reasonQueryTruncated

		case agentTruncated(query):
			// Cut by activitySQL's own left() instead. Same consequence, different cap.
			c.textReason = reasonTextTruncated

		case activityParameter.MatchString(query):
			// Parameterized, so not literal-eligible, but better generic input than
			// nothing when the statements view had none.
			if c.genericText == "" {
				c.genericText = query
			}

		default:
			c.literalText = query
		}
	}
}

// matchActivity picks one session per candidate: active first, then most recent
// query_start. A fallback candidate is already a session and matches on pid.
func matchActivity(c *explainCandidate, activity []activityRow) (activityRow, bool) {
	var best activityRow

	found := false

	for _, row := range activity {
		if row.pid == nil || !activityMatches(c, row) {
			continue
		}

		if !found || betterActivityMatch(row, best) {
			best, found = row, true
		}
	}

	return best, found
}

func activityMatches(c *explainCandidate, row activityRow) bool {
	if c.source == candidateSourceActivity {
		return c.pid != nil && *row.pid == *c.pid
	}

	// (query_id, datid, usesysid), never query_id alone: pg_stat_activity is cluster-wide
	// and one queryid appears under several roles, so an integer-only join would hand this
	// candidate another role's session text - the wrong plan, and that role's literals
	// under this candidate's name. datid is redundant while identify succeeded, and
	// checked anyway because a failed identify admits other-database candidates.
	// toplevel is unprovable from the activity side and does not participate.
	return c.queryid != nil && row.queryID != nil && *row.queryID == *c.queryid &&
		text(row.usesysid) == c.userid &&
		text(row.datid) == c.dbid
}

func betterActivityMatch(row, best activityRow) bool {
	rowActive := text(row.state) == "active"
	bestActive := text(best.state) == "active"

	if rowActive != bestActive {
		return rowActive
	}

	if row.queryStart == nil {
		return false
	}

	return best.queryStart == nil || row.queryStart.After(*best.queryStart)
}

// truncatedActivityText applies the server's own cap. The cut is unmarked and not a
// single length: pgstat_clip_activity trims to track_activity_query_size-1 bytes and then
// back to a character boundary, so a truncated string lands anywhere in
// [size - max_char_bytes, size - 1] (measured at 1020-1023 under the 1kB default).
// The whole band counts as truncated; a genuine statement inside it downgrades to the
// generic mode, which is the direction this has to fail in.
func truncatedActivityText(row activityRow, facts activityFacts) bool {
	if row.queryBytes == nil || facts.activityQuerySize <= 0 {
		return false
	}

	slack := facts.maxCharBytes
	if slack < 1 {
		slack = 1
	}

	return *row.queryBytes >= facts.activityQuerySize-slack
}

// agentTruncated reports the cap+1 sentinel both text reads ask the server for: one rune
// past DefaultMaxQueryText means the agent cut it, so the prefix ends mid-token.
func agentTruncated(query string) bool {
	return utf8.RuneCountInString(query) > DefaultMaxQueryText
}

// statementQueryText is the statements view's text, dropped when the agent's own cap cut
// it. The generic mode has no other input, so a cut here is reason=text_truncated.
func statementQueryText(query *string) string {
	if q := text(query); !agentTruncated(q) {
		return q
	}

	return ""
}

// statementTextReason names which cut applied - worth telling apart, since
// track_activity_query_size is a GUC the reader can raise and DefaultMaxQueryText is not.
func statementTextReason(query *string) string {
	if agentTruncated(text(query)) {
		return reasonTextTruncated
	}

	return reasonQueryTruncated
}

// explainable is the allowlist, applied to the leading keyword after comments and an
// opening parenthesis are stripped.
func explainable(query string) bool {
	return slices.Contains(explainableKeywords, leadingKeyword(query))
}

// leadingKeyword strips leading whitespace, SQL comments and '(' - a parenthesised
// SELECT is still a SELECT - and uppercases the first word. Block comments nest in
// PostgreSQL, so the depth is counted rather than matched to the first close.
func leadingKeyword(query string) string {
	rest := query

	for {
		trimmed := strings.TrimLeft(rest, " \t\r\n(")

		switch {
		case strings.HasPrefix(trimmed, "--"):
			_, after, found := strings.Cut(trimmed, "\n")
			if !found {
				return ""
			}

			rest = after

		case strings.HasPrefix(trimmed, "/*"):
			after, ok := skipBlockComment(trimmed)
			if !ok {
				return ""
			}

			rest = after

		default:
			word := trimmed
			if at := strings.IndexAny(word, " \t\r\n(;"); at >= 0 {
				word = word[:at]
			}

			return strings.ToUpper(word)
		}
	}
}

func skipBlockComment(s string) (string, bool) {
	depth, i := 0, 0

	for i < len(s)-1 {
		switch {
		case s[i] == '/' && s[i+1] == '*':
			depth++
			i += 2

		case s[i] == '*' && s[i+1] == '/':
			depth--
			i += 2

			if depth == 0 {
				return s[i:], true
			}

		default:
			i++
		}
	}

	return "", false
}

// --- the activity read -------------------------------------------------------

type activityRow struct {
	pid         *int32
	queryID     *int64
	datid       *string
	usesysid    *string
	state       *string
	query       *string
	queryBytes  *int64
	queryStart  *time.Time
	stateChange *time.Time

	// runningFor and ranFor are computed server-side, so the agent never subtracts its
	// own clock from the server's.
	runningFor *float64
	ranFor     *float64
}

type activityFacts struct {
	read bool

	searchPath         string
	selfOID            string
	autoExplainVisible bool
	activityQuerySize  int64

	// maxCharBytes is the server encoding's widest character; it widens the truncation
	// gate, whose clip lands on a character boundary at or below the byte cap.
	maxCharBytes int64
}

// readActivity takes the one read, under the ordinary statement bound. The error is
// returned, not swallowed: a failure costs the literal mode its text, the ranking its
// self-exclusion and the summary its facts, which would otherwise read as an idle
// database.
func readActivity(ctx context.Context, q RowQuerier) ([]activityRow, activityFacts, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, activitySQL, MaxActivitySessions, DefaultMaxQueryText+1)
	if err != nil {
		return nil, activityFacts{}, err
	}
	defer rows.Close()

	var (
		activity []activityRow
		facts    activityFacts
	)

	for rows.Next() {
		var (
			row               activityRow
			searchPath        *string
			selfOID           *string
			visible           *bool
			activityQuerySize *int64
			maxCharBytes      *int64
		)

		if err := rows.Scan(
			&searchPath, &selfOID, &visible, &activityQuerySize, &maxCharBytes,
			&row.pid, &row.queryID, &row.datid, &row.usesysid, &row.state,
			&row.query, &row.queryBytes, &row.queryStart, &row.stateChange,
			&row.runningFor, &row.ranFor,
		); err != nil {
			return activity, facts, err
		}

		facts = activityFacts{
			read:               true,
			searchPath:         text(searchPath),
			selfOID:            text(selfOID),
			autoExplainVisible: visible != nil && *visible,
			activityQuerySize:  int64Value(activityQuerySize),
			maxCharBytes:       int64Value(maxCharBytes),
		}

		activity = append(activity, row)
	}

	if err := rows.Err(); err != nil {
		return activity, facts, err
	}

	return activity, facts, nil
}

// --- submission --------------------------------------------------------------

// submitAll runs the explain pass under a server-side timeout, reset afterwards even
// when a candidate failed, so the shared connection is handed back at its default.
func (e *Explain) submitAll(
	ctx context.Context, q RowQuerier, s SampleContext,
	candidates []*explainCandidate, counters *explainCounters,
) {
	if len(candidates) == 0 {
		return
	}

	runUtilityStatement(ctx, q, setExplainTimeoutSQL)
	defer runUtilityStatement(ctx, q, resetExplainTimeoutSQL)

	started := e.now()

	for _, c := range candidates {
		// Already carrying the server's own plan; the estimated modes would add nothing
		// and cost the budget.
		if c.mode != planModeNone {
			continue
		}

		if e.now().Sub(started) >= ExplainBudget {
			c.reason = reasonBudgetSpent
			counters.skippedBudget++

			continue
		}

		e.submitOne(ctx, q, s, c)
	}
}

// submitOne picks the mode from the text that is available, not from a rule, and sends it
// over the protocol that mode requires.
func (e *Explain) submitOne(
	ctx context.Context, q RowQuerier, s SampleContext, c *explainCandidate,
) {
	mode, generic, ok := choosePlanMode(c, s)
	if !ok {
		c.reason = mode

		return
	}

	statement := explainStatement(explainOptions(generic), submittableText(c, generic))

	plan, truncated, err := submitExplain(ctx, q, statement, generic)
	if err != nil {
		c.err = s.errorText(err)

		return
	}

	c.mode = mode
	c.plan = plan
	c.planTruncated = truncated
	c.planQueryID = planQueryIdentifier(plan)

	// Only the literal mode can assert equality: the generic text jumbles a Param where
	// the original jumbled a Const, so its identifier differs by construction.
	if !generic && c.planQueryID != "" && c.queryid != nil {
		c.queryIDMatch = strconv.FormatBool(c.planQueryID == strconv.FormatInt(*c.queryid, 10))
	}
}

// choosePlanMode returns the mode, whether it needs GENERIC_PLAN, and false with a
// reason in place of the mode when nothing can be submitted. An unusable literal text
// does not end the candidate: the generic mode is still tried.
func choosePlanMode(c *explainCandidate, s SampleContext) (mode string, generic, ok bool) {
	if c.literalText != "" && !multiStatement(c.literalText) {
		return planModeEstimatedLiteral, false, true
	}

	switch {
	case c.genericText == "":
		if c.literalText != "" {
			return reasonMultiStatement, false, false
		}

		return c.textReason, false, false

	case !s.HasGenericPlan:
		return reasonGenericPlanUnsupported, false, false

	case multiStatement(c.genericText):
		return reasonMultiStatement, true, false
	}

	return planModeEstimatedGeneric, true, true
}

// multiStatement reports whether text carries more than one command, ignoring a trailing
// separator. Deliberately crude: a ';' inside a string literal costs one candidate a plan,
// while a false negative would hand the simple protocol a batch to execute.
func multiStatement(query string) bool {
	return strings.Contains(strings.TrimRight(strings.TrimSpace(query), ";"), ";")
}

func submittableText(c *explainCandidate, generic bool) string {
	if generic {
		return c.genericText
	}

	return c.literalText
}

// explainOptions is the only place an option is added, and ANALYZE is not in it: it
// executes the statement, which here is the most expensive query on the server.
// default_transaction_read_only is not the control - measured, it stops EXPLAIN ANALYZE
// UPDATE and lets EXPLAIN ANALYZE SELECT count(*) run.
func explainOptions(generic bool) []string {
	options := []string{"VERBOSE", "SETTINGS"}

	if generic {
		options = append(options, "GENERIC_PLAN")
	}

	return options
}

func explainStatement(options []string, query string) string {
	return "EXPLAIN (" + strings.Join(options, ", ") + ") " + query
}

// simpleQuerier is the raw simple query protocol, an optional interface so the shared
// RowQuerier does not grow a method only this collector uses.
type simpleQuerier interface {
	ExecSimple(ctx context.Context, sql string, maxBytes int) ([]string, bool, error)
}

// errNoSimpleProtocol is a wiring failure, not a server one: the generic mode cannot be
// submitted over anything else.
var errNoSimpleProtocol = errors.New("the generic plan mode needs the simple query protocol")

// submitExplain returns the server's bytes verbatim, one plan line per row, bounded at
// MaxPlanBytes; the second return is what the block records as plan_truncated=.
//
// The literal mode goes extended (QueryExecModeExec - one-shot, so customer SQL never
// enters pgx's statement cache), whose refusals of a multi-statement string and of an
// unbound $n are both features.
//
// The generic mode needs the raw simple query protocol: an unbound $1 fails at Bind
// before the server sees the statement, and pgx's QueryExecModeSimpleProtocol substitutes
// $n client-side and rejects it with "insufficient arguments" before the wire.
func submitExplain(ctx context.Context, q RowQuerier, statement string, generic bool) (
	[]byte, bool, error,
) {
	if generic {
		simple, ok := q.(simpleQuerier)
		if !ok {
			return nil, false, errNoSimpleProtocol
		}

		lines, truncated, err := simple.ExecSimple(ctx, statement, MaxPlanBytes)
		if err != nil {
			return nil, false, err
		}

		return planBytes(lines), truncated, nil
	}

	rows, err := q.Query(ctx, statement, pgx.QueryExecModeExec)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	var (
		lines     []string
		held      int
		truncated bool
	)

	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, false, err
		}

		// Past the cap the rows are still scanned and dropped, never left unread: the
		// statement has to finish on a connection nine other artifacts share.
		if held >= MaxPlanBytes {
			truncated = true

			continue
		}

		held += len(line) + 1

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	return planBytes(lines), truncated, nil
}

func planBytes(lines []string) []byte {
	var plan strings.Builder

	for _, line := range lines {
		plan.WriteString(line)
		plan.WriteByte('\n')
	}

	return []byte(plan.String())
}

// planQueryIdentifier reads VERBOSE's Query Identifier: value, absent below PostgreSQL 16
// and under a non-text log_format, where the block then carries no plan_queryid= at all.
// Bounded to the integer, not the line: csvlog and jsonlog put the rest of the record on
// the same physical line.
func planQueryIdentifier(plan []byte) string {
	// The last occurrence, not the first: the identifier is a plan's final line, while the
	// entry opens with the customer's own Query Text:, which could contain the literal
	// string. A line anchor cannot help - jsonlog is one physical line.
	matches := planIdentifier.FindAllSubmatch(plan, -1)
	if len(matches) == 0 {
		return ""
	}

	return string(matches[len(matches)-1][1])
}

var planIdentifier = regexp.MustCompile(`Query Identifier:\s*(-?[0-9]+)`)

// --- blocks ------------------------------------------------------------------

func (e *Explain) writeCandidate(
	w io.Writer, s SampleContext, c *explainCandidate, facts activityFacts,
) error {
	fields := []headerField{
		{"rank", strconv.Itoa(c.rank)},
		{"candidate_source", c.source},
	}

	if c.source == candidateSourceActivity {
		// pid and query_start identify a candidate the statements view never saw;
		// query_id rides whenever the server populated it, extension view or not.
		fields = append(fields,
			headerField{"pid", int32Text(c.pid)},
			headerField{"query_start", timeText(c.queryStart)},
		)

		if c.queryid != nil {
			fields = append(fields, headerField{"query_id", strconv.FormatInt(*c.queryid, 10)})
		}
	} else {
		fields = append(fields, headerField{"queryid", int64Text(c.queryid)})
	}

	fields = append(fields, headerField{"mode", c.mode})

	if c.matchedBy != "" {
		fields = append(fields, headerField{"matched_by", c.matchedBy})
	}

	if c.planQueryID != "" {
		fields = append(fields, headerField{"plan_queryid", c.planQueryID})
	}

	if c.queryIDMatch != "" {
		fields = append(fields, headerField{"queryid_match", c.queryIDMatch})
	}

	// Only where more than one execution carried this identifier: the block's body is
	// the slowest of them, and one is the uninteresting case.
	if c.plansSeen > 1 {
		fields = append(fields, headerField{"plans_seen", strconv.Itoa(c.plansSeen)})
	}

	if facts.read {
		fields = append(fields, headerField{"search_path", facts.searchPath})
	}

	if c.planTruncated {
		fields = append(fields, headerField{"plan_truncated", "true"})
	}

	switch {
	case c.err != "":
		fields = append(fields, headerField{"error", c.err})

	case c.reason != "":
		fields = append(fields, headerField{"reason", c.reason})

	case c.mode == planModeNone:
		// Mode logged ranks candidates but submits nothing, so one with no attachable
		// log entry has no plan.
		fields = append(fields, headerField{"reason", reasonNoLoggedPlan})
	}

	return e.writeBlock(w, s, fields, c.plan)
}

// writePlan writes a stored plan no candidate is carrying. An empty queryid= is
// present rather than omitted: it means "no pg_stat_statements row", where a missing key
// would mean nobody looked.
func (e *Explain) writePlan(w io.Writer, s SampleContext, plan *loggedPlan) error {
	fields := []headerField{
		{"queryid", ""},
		{"mode", planModeLogged},
		{"matched_by", matchedByMessage},
	}

	reason := reasonNoQueryIdentifier

	if plan.queryID != "" {
		fields = append(fields, headerField{"plan_queryid", plan.queryID})

		// Reached only from the closing pass: the ranked report never ran, so there was
		// nothing to attach this to - which is not the same as having no identifier.
		reason = reasonNoRankedReport
	}

	fields = append(fields, headerField{"reason", reason})

	return e.writeBlock(w, s, fields, plan.body)
}

// writeSummary is the collector's own administrative block: the window's field set is
// fixed and has no hook for one, as with the log tails' drain blocks.
func (e *Explain) writeSummary(
	w io.Writer, s SampleContext, selection explainSelection, facts activityFacts,
	store *planStore, read *tailRead, activityErr string,
) error {
	fields := []headerField{
		{"summary", "true"},
		{"ranking", selection.ranking},
	}

	if len(selection.candidates) == 0 {
		fields = append(fields, headerField{"reason", reasonNoCandidates})
	}

	fields = append(fields, selection.counters.fields()...)

	if store.ambiguous > 0 {
		fields = append(fields,
			headerField{"plans_ambiguous", strconv.Itoa(store.ambiguous)})
	}

	fields = append(fields, e.logFields(store, read)...)

	if facts.read {
		fields = append(fields,
			headerField{"auto_explain_visible", strconv.FormatBool(facts.autoExplainVisible)})
	}

	// Last, and only on failure: without it a read that never happened is
	// indistinguishable from an idle database.
	if activityErr != "" {
		fields = append(fields, headerField{"activity_error", activityErr})
	}

	return e.writeBlock(w, s, fields, nil)
}

// logFields reports the plan store in the log-tail engine's own vocabulary. A tail that
// never resolved gets its reason and capture mode and no count, as writeReasonBlock does:
// a zero beside a reason renders an absence as a measurement. The reason rides
// log_reason= because the ranking already owns reason=.
func (e *Explain) logFields(store *planStore, read *tailRead) []headerField {
	source := e.tail.source

	if source.reason != "" {
		return []headerField{
			{"log_reason", source.reason},
			{"capture_mode", source.captureMode()},
		}
	}

	fields := []headerField{
		{"capture_mode", source.captureMode()},
	}

	// The engine's own keys, in its own spelling, because the server already parses them
	// from two artifacts. Without them a read that overran MaxScanBytes - which seeks to
	// EOF and returns no events - reports plans_harvested=0, exactly what a cluster with
	// no auto_explain reports.
	if read != nil {
		fields = append(fields, readStateFields(read)...)
	}

	fields = append(fields, headerField{"plans_harvested", strconv.Itoa(store.total)})

	// Non-zero, these two separate "the log held nothing" from "the caps bound".
	if store.written > 0 {
		fields = append(fields, headerField{"plans_written", strconv.Itoa(store.written)})
	}

	if store.dropped > 0 {
		fields = append(fields, headerField{"plans_dropped", strconv.Itoa(store.dropped)})
	}

	return fields
}

// writeBlock renders header and body as one Write. bytes= is the body's length and
// the only end marker a reader gets: plan text and Query Text: lines are multi-line
// and can legally contain a leading '#'.
func (e *Explain) writeBlock(w io.Writer, s SampleContext, fields []headerField, body []byte) error {
	header := make([]headerField, 0, len(fields)+3)
	header = append(header,
		headerField{"db", s.Database},
		headerField{"dbid", s.DBID},
	)
	header = append(header, fields...)
	header = append(header, headerField{"bytes", strconv.Itoa(len(body))})

	// One buffer, one Write: a header that lands without its body leaves every block
	// after it unparseable.
	var block bytes.Buffer

	if err := writeBlockHeaderFormat(&block, "pg_explain", "database", formatText, header, s.At); err != nil {
		return err
	}

	block.Write(body)

	_, err := w.Write(block.Bytes())

	return err
}

func floatValue(v *float64) float64 {
	if v == nil {
		return 0
	}

	return *v
}

func int64Value(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}

// explainMatch selects auto_explain's entries and nothing else. auto_explain and
// log_min_duration_statement both open "duration: <n> ms ", so only what ends the first
// line separates them. 00000 is registered paired because it is every LOG line in the
// file: without it the SQLSTATE match passes everything on csvlog and jsonlog, and the
// message predicate never runs.
var explainMatch = eventMatch{
	sqlstate:      []string{"00000"},
	paired:        []string{"00000"},
	message:       []string{"duration: "},
	messageSuffix: []string{"plan:"},
}
