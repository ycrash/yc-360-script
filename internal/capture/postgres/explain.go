package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
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
	// DefaultMaxExplains bounds how many shapes one sample attempts; the rest wait for
	// the next sample, so a database that walks in tracking thousands of shapes is
	// explained as a drip rather than a burst. Not configurable: a tunable N produces
	// plan counts nobody can reproduce from a bundle.
	DefaultMaxExplains = 10

	// ExplainTimeout bounds one statement, applied server-side with
	// SET statement_timeout / RESET and never as a client context: pgx closes the
	// connection when a context expires and this window never reconnects. Server-side,
	// a timeout is an ordinary error= and the next candidate proceeds. Either estimated
	// tier sends five statements per candidate, and only its EXPLAIN plans anything;
	// the other four are catalogue work that finishes in the round trip.
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

	// reasonNoCandidates: nothing new to attempt this sample - a feed with no unseen
	// shapes, or no feed at all - not a failure.
	reasonNoCandidates = "no_candidates"

	// reasonStatementsUnread: the statements collector offered no read this sample, so
	// the feed could not be walked. Rides statements_reason=, beside the extension's
	// own reasons and a failed read's error text.
	reasonStatementsUnread = "statements_unread"

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
	// closing sample never ran and no candidate was left to attach it to. The token
	// predates the once-per-shape selection and is kept as written.
	reasonNoRankedReport = "no_ranked_report"
)

// Why the literal tier did not apply, written as literal_reason= on every block whose
// attempt fell to the generic tier and on a sample's summary when the tier was off.
// The first three are the source-side cap the agent observed (D7): the tier runs only
// under a finite log_parameter_max_length, which is the deployment's to set and never
// the agent's. The rest are about this candidate's evidence in the log.
const (
	// reasonParameterCapUnbounded: log_parameter_max_length is -1, the default. Values
	// would arrive whole, and the agent must not retain what nothing bounds.
	reasonParameterCapUnbounded = "parameter_cap_unbounded"

	// reasonParametersNotLogged: log_parameter_max_length is 0, so the server writes
	// no parameters at all.
	reasonParametersNotLogged = "parameters_not_logged"

	// reasonParameterCapUnread: the facts read failed or the setting was absent, so the
	// cap is unknown, which is treated as unbounded.
	reasonParameterCapUnread = "parameter_cap_unread"

	// reasonNoBindRecord: the tier was on and the log yielded no execute record with
	// parameters for this identifier.
	reasonNoBindRecord = "no_bind_record"

	// reasonBindClaimed: the same identifier under another role already took the
	// record; the log names an identifier and nothing else.
	reasonBindClaimed = "bind_record_claimed"

	// reasonBindTruncated: the record's values carried the server's clip marker, so the
	// set is incomplete.
	reasonBindTruncated = "bind_record_truncated"

	// reasonBindMalformed: the record did not parse - a missing or duplicate position,
	// or quoting the parser could not follow.
	reasonBindMalformed = "bind_record_malformed"
)

// planDuration reads the duration the entry declares; the slowest execution is the
// pathological plan, and the one worth keeping.
var planDuration = regexp.MustCompile(`duration:\s+([0-9.]+)\s+ms`)

// setExplainTimeoutSQL is formatted from the constant so the literal cannot drift.
var setExplainTimeoutSQL = fmt.Sprintf("SET statement_timeout TO '%dms'",
	ExplainTimeout.Milliseconds())

const resetExplainTimeoutSQL = `RESET statement_timeout`

// The estimated tiers' session setting. plan_cache_mode is consulted when a prepared
// statement is executed, so forcing it decides the plan EXPLAIN EXECUTE returns: under
// force_generic_plan the NULLs the generic tier binds cannot select a custom plan and
// the plan keeps its $n symbols, on 14 and 15 as on 16 and later; under
// force_custom_plan the values the literal tier binds are planned as the values they
// are, whatever the session's own plan_cache_mode says. Measured on every version of
// the matrix: under the default auto, the generic EXECUTE folds the NULLs into
// "One-Time Filter: false". SET/RESET, as statement_timeout is.
const (
	forceGenericPlanSQL   = `SET plan_cache_mode TO force_generic_plan`
	forceCustomPlanSQL    = `SET plan_cache_mode TO force_custom_plan`
	resetPlanCacheModeSQL = `RESET plan_cache_mode`
)

// preparedStatementPrefix names the agent's own prepared statements. The suffix is a
// counter across the window, so a DEALLOCATE that failed cannot collide with the next
// PREPARE, and the name says whose it is to anyone reading pg_prepared_statements.
const preparedStatementPrefix = "yc_explain_"

// explainFactsSQL is one read per sample, carrying four facts that cost no extra
// statement: search_path, the capture role's OID (self-exclusion), auto_explain
// visibility and log_parameter_max_length, the source-side cap the literal tier is
// gated on. They come from pg_settings and pg_roles, never current_setting() or
// ::regrole, both of which fail the whole statement - on a role name needing quotes,
// and on a name this role cannot see. The cap is NULL where the setting is absent,
// which the gate treats as unread.
const explainFactsSQL = `SELECT current_setting('search_path'),
       (SELECT oid::text FROM pg_catalog.pg_roles
         WHERE rolname = current_user),
       EXISTS (SELECT 1 FROM pg_catalog.pg_settings
                WHERE name = 'auto_explain.log_min_duration'),
       (SELECT setting::bigint FROM pg_catalog.pg_settings
         WHERE name = 'log_parameter_max_length')`

// parameterSymbol is a $n placeholder: what pg_stat_statements writes for every
// constant it normalized, and what plain EXPLAIN refuses with "there is no parameter
// $1". No parser is needed because the scan errs one way only: a stray '$9' in a
// string inflates one candidate's count, while a real parameter can never hide.
var parameterSymbol = regexp.MustCompile(`\$([0-9]+)`)

// inferredParameters is how many NULLs the generic tier binds: the highest $n in the
// text. The normalized text numbers its placeholders without gaps, so the highest is
// the count; where a stray '$9' in a comment inflates it, the server answers "wrong
// number of parameters" and the block records that, not a plan for a different shape.
func inferredParameters(query string) int {
	highest := 0

	for _, match := range parameterSymbol.FindAllStringSubmatch(query, -1) {
		n, err := strconv.Atoi(match[1])
		if err == nil && n > highest {
			highest = n
		}
	}

	return highest
}

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

	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// sq offers each sample's statements read; Explain never re-runs it.
	sq *SlowQueries

	tail logTail

	// store keeps the log's plans across samples, so a shape the cap deferred can
	// still claim the execution the server logged for it; binds keeps the log's bind
	// records the same way, for the literal tier.
	store *planStore
	binds *bindStore

	// prefix is the stderr log_line_prefix compiled for %Q, built once the tail's
	// settings are known.
	prefix *linePrefix

	// seen is every statement key the feed has shown, with the sample it first
	// appeared in; queue holds the ones not yet attempted, in that order.
	seen  map[statementKey]int
	queue []*explainCandidate

	// now is the budget's clock, injectable so a test can spend it.
	now func() time.Time

	// reported gates WriteClosing: the closing sample already emitted everything.
	reported bool

	// tailOpened records that the opening sample ran; false is the connect-failure path,
	// where nothing was attempted.
	tailOpened bool

	// prepared counts the estimated tiers' PREPAREs, which is the statement name's
	// suffix.
	prepared int
}

// NewExplain panics on a nil SlowQueries or an unrecognised mode: both are wiring bugs
// at a call site config already validated, and treating a bad mode as off would write
// reason=explain_disabled about a run that asked for plans.
func NewExplain(mode string, sq *SlowQueries) *Explain {
	if sq == nil {
		panic("postgres: NewExplain requires the SlowQueries collector whose reads it walks")
	}

	if mode != "" && mode != ExplainModeLogged && mode != ExplainModeAll {
		panic("postgres: NewExplain got an unvalidated explain mode: " + mode)
	}

	return &Explain{
		mode:  mode,
		sq:    sq,
		tail:  newLogTail("pg_explain", explainMatch),
		store: newPlanStore(nil),
		binds: newBindStore(),
		seen:  map[statementKey]int{},
		now:   time.Now,
	}
}

func (e *Explain) Artifact() Artifact {
	return Artifact{
		Name:     "pg_explain",
		FileName: "pg_explain.txt",

		// database, not cluster: plans are obtainable only for the connected database,
		// even though the feed is cluster-wide.
		Scope: "database",

		// The run's cadence: every sample walks that tick's statements read for shapes
		// not yet seen, and the close is the last chance to attempt what waited.
		Schedule: Periodic(e.Interval),
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
	// its full server-side timeout, plus the one facts read.
	return ExplainBudget + ExplainTimeout + StatementTimeout
}

func (e *Explain) enabled() bool { return e.mode == ExplainModeLogged || e.mode == ExplainModeAll }

// submits reports whether this run sends statements to the server; only mode all does.
func (e *Explain) submits() bool { return e.mode == ExplainModeAll }

// Sample arms the log tail on the opening sample and reports on every sample: the
// once-per-shape rule is applied in the interval a shape is first seen, so each tick
// walks the feed, drains the log and attempts what is waiting.
func (e *Explain) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	if !e.enabled() {
		// Disabled is one block at the close and no summary: a summary of zeroes would
		// read as an empty database rather than a feature left off.
		if s.Index < s.Total {
			return nil
		}

		return e.writeBlock(w, s, []headerField{{"reason", reasonExplainDisabled}}, nil)
	}

	if s.Index == 1 {
		// Registration order puts this after the other collectors' t0 statements,
		// so the tail opens past the agent's own first burst of plans.
		e.tailOpened = true

		e.tail.openAtEnd(ctx, q, s)
	}

	return e.report(ctx, q, w, s)
}

// report writes one sample's blocks: the shapes attempted this tick, the
// identifier-less plans the log yielded since the last one, and the summary.
func (e *Explain) report(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	closing := s.Index == s.Total
	e.reported = closing

	// The facts come first: the literal tier's gate is one of them, and it decides
	// whether this read's bind records are kept at all.
	facts, factsErr := readExplainFacts(ctx, q)
	gate := e.literalGate(facts)

	events, read := e.tail.readEvents(ctx, q, time.Time{})
	if closing {
		e.tail.closeFile()
	}

	e.ingest(events, e.submits() && gate == "")

	feed, fed := e.sq.feed(s)
	selection := e.selectCandidates(s, feed, fed, facts)

	// Attachment runs before submission, in tier order: the LOGGED mode is the plan for
	// the execution that actually happened, so a candidate it claims is never also
	// submitted, and a bind record makes the submission literal rather than generic.
	e.store.attach(selection.candidates)

	if e.submits() {
		e.binds.attach(selection.candidates, gate)
		e.submitAll(ctx, q, s, selection.candidates, &selection.counters)
	}

	for _, c := range e.requeueSkipped(&selection) {
		if err := e.writeCandidate(w, s, c, facts); err != nil {
			return err
		}
	}

	// Identifier-less plans follow this sample's blocks and leave the store. An
	// identified plan no candidate claimed stays retained for a shape still queued,
	// showing meanwhile as plans_harvested= minus plans_written=.
	for _, plan := range e.store.takeUnattached() {
		if err := e.writePlan(w, s, plan); err != nil {
			return err
		}

		e.store.written++
	}

	return e.writeSummary(w, s, selection, facts, gate, read, s.errorText(factsErr))
}

// ingest sorts the tail's events into the two stores: a log_min_duration_statement
// execute record carrying parameters is bind evidence, and everything else the matcher
// passed is an auto_explain plan. retain is whether bind records are kept - the
// literal tier on, and the source-side cap finite; otherwise they are counted and
// their values dropped here.
func (e *Explain) ingest(events [][]byte, retain bool) {
	for _, event := range events {
		if entry, ok := parseLogEntry(event, e.tail.source.format, e.linePrefix()); ok {
			if record, ok := executeRecord(entry); ok {
				e.binds.add(record, retain)

				continue
			}
		}

		e.store.add(event)
	}
}

// linePrefix compiles the tail's log_line_prefix once it is known.
func (e *Explain) linePrefix() *linePrefix {
	if !e.tail.haveSettings {
		return nil
	}

	if e.prefix == nil || e.prefix.template != e.tail.settings.logLinePrefix {
		e.prefix = compileLinePrefix(e.tail.settings.logLinePrefix)
	}

	return e.prefix
}

// literalGate is D7 applied: the literal tier runs only under a finite
// log_parameter_max_length observed on this connection, and the reason it does not is
// written. The observed value is the agent session's, which the deployment's own
// acceptance test proves for the workload role; the agent never sets it.
func (e *Explain) literalGate(facts explainFacts) string {
	switch {
	case !facts.read || !facts.parameterCapRead:
		return reasonParameterCapUnread

	case facts.parameterCap < 0:
		return reasonParameterCapUnbounded

	case facts.parameterCap == 0:
		return reasonParametersNotLogged
	}

	return ""
}

// requeueSkipped puts back, at the head of the queue and in order, every candidate the
// aggregate budget skipped: its attempt never began, so it is not this sample's and no
// block is written for it. It returns what was attempted.
func (e *Explain) requeueSkipped(selection *explainSelection) []*explainCandidate {
	var attempted, skipped []*explainCandidate

	for _, c := range selection.candidates {
		if c.reason == reasonBudgetSpent {
			c.reason = ""
			skipped = append(skipped, c)

			continue
		}

		attempted = append(attempted, c)
	}

	if len(skipped) > 0 {
		e.queue = append(skipped, e.queue...)
		selection.counters.written = len(attempted)
		selection.counters.queued = len(e.queue)
	}

	selection.candidates = attempted

	return attempted
}

// WriteClosing acts only where the closing sample did not, so a cancelled or
// deadline-exceeded window still ships what the tail collected and says what was left
// waiting. No connection here, so it never re-resolves and never follows a rotation.
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

	// Nothing is attempted here, so a bind record has no use and is not kept.
	e.ingest(events, false)

	for _, plan := range e.store.takeAll() {
		if err := e.writePlan(w, s, plan); err != nil {
			return err
		}

		e.store.written++
	}

	selection := explainSelection{drain: true}
	selection.counters.queued = len(e.queue)

	return e.writeSummary(w, s, selection, explainFacts{}, "", read, "")
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

	store.addAll(events)

	return store
}

func (p *planStore) addAll(events [][]byte) {
	for _, event := range events {
		p.add(event)
	}
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

		if p.claimed[id] {
			// The same queryid under a different role. The entry names an identifier and
			// nothing else, so neither candidate can be shown to be the logged execution,
			// and attaching to both would write one plan under two headers. The first
			// keeps it; the rest take their ordinary path.
			p.ambiguous++

			continue
		}

		plan, ok := p.byID[id]
		if !ok {
			continue
		}

		p.claimed[id] = true
		p.written++

		// The body is written under the candidate, so the store need not hold it
		// against MaxRetainedPlanBytes any longer; claimed keeps the identifier.
		p.remove(plan)

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

// unattached is what a sample's report writes after its candidates: the entries the
// log gave no identifier for. An identified plan no candidate claimed stays retained
// for a shape still queued.
func (p *planStore) unattached() []*loggedPlan {
	var plans []*loggedPlan

	for _, plan := range p.retained {
		if plan.queryID == "" {
			plans = append(plans, plan)
		}
	}

	return plans
}

// takeUnattached is unattached and their removal, so a plan written under one sample
// is not written again under the next.
func (p *planStore) takeUnattached() []*loggedPlan {
	plans := p.unattached()

	for _, plan := range plans {
		p.remove(plan)
	}

	return plans
}

// takeAll is the closing pass's set, and the store emptied: no report to attach to,
// so everything is written.
func (p *planStore) takeAll() []*loggedPlan {
	plans := p.retained

	p.retained = nil
	p.byID = map[string]*loggedPlan{}
	p.bytes = 0

	return plans
}

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

// explainCounters is the summary block's account of one sample's selection, which is
// otherwise invisible: which rows were dropped, and for what. Exclusions are counted
// when a shape is first seen; masked rows carry no identity, so they are counted on
// every read.
type explainCounters struct {
	considered int

	excludedOtherDatabase int
	excludedMasked        int
	excludedUtility       int
	excludedNotToplevel   int
	excludedSelf          int

	// observed is first seen this sample and eligible; written is attempted this
	// sample; skippedBudget went back to the queue when the aggregate ran out; queued
	// is what still waits after this sample.
	observed      int
	written       int
	skippedBudget int
	queued        int
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
		{"candidates_new", c.observed},
		{"candidates_written", c.written},
		{"candidates_skipped_budget", c.skippedBudget},
		{"candidates_queued", c.queued},
	}

	fields := make([]headerField, 0, len(counted))

	for _, f := range counted {
		if f.value > 0 {
			fields = append(fields, headerField{f.key, strconv.Itoa(f.value)})
		}
	}

	return fields
}

// explainCandidate is one shape to attempt and everything the block about it says.
type explainCandidate struct {
	queryid *int64

	// userid and dbid complete the identity: queryid alone is not unique across roles or
	// databases. The log's records name the identifier alone, which is why the first
	// candidate to claim one keeps it.
	userid string
	dbid   string

	// firstSeen is the sample the feed first showed this shape. The block is written
	// in the sample it was attempted, which the cap can put later.
	firstSeen int

	// genericText is the normalized text, $n placeholders and all; literal is the
	// log's own record for this shape, whose text and values the literal tier
	// prepares. Either may be absent.
	genericText string
	literal     *bindRecord

	// parameters is how many arguments the attempt bound - one NULL per placeholder in
	// the generic tier, one decoded value each in the literal - written as parameters=
	// on every block whose attempt went through a prepared statement, succeeded or not.
	parameters int
	prepared   bool

	// literalReason says why the literal tier did not apply, on every attempt that
	// fell to the generic tier.
	literalReason string

	// textReason says which cut left this candidate with nothing to submit.
	textReason string

	mode   string
	reason string
	err    string

	planQueryID  string
	queryIDMatch string

	// matchedBy and plansSeen ride the LOGGED mode only; bindsSeen the literal.
	matchedBy string
	plansSeen int
	bindsSeen int

	plan []byte

	// planTruncated: the plan came back past MaxPlanBytes and the body is a prefix.
	planTruncated bool
}

// explainSelection is one sample's attempt list and its account.
type explainSelection struct {
	candidates []*explainCandidate
	counters   explainCounters

	// feedReason is why the feed had no rows this sample.
	feedReason string

	// drain marks the closing pass, which attempts nothing.
	drain bool
}

// selectCandidates queues every shape the feed shows for the first time, in the feed's
// own order, and attempts the first DefaultMaxExplains of what is waiting. No ranking:
// which shapes matter is the server's judgment. The cap and the aggregate budget bound
// each sample, and the summary says how many still wait.
func (e *Explain) selectCandidates(
	s SampleContext, feed statementFeed, fed bool, facts explainFacts,
) explainSelection {
	var selection explainSelection

	counters := &selection.counters

	switch {
	case !fed:
		selection.feedReason = reasonStatementsUnread

	case !feed.read:
		selection.feedReason = feed.reason

	default:
		counters.considered = len(feed.rows)

		for _, row := range feed.rows {
			if row.queryid == nil {
				counters.excludedMasked++

				continue
			}

			key, _ := statementRowKey(row)

			if _, seen := e.seen[key]; seen {
				continue
			}

			e.seen[key] = s.Index

			if !eligible(row, s, facts, counters) {
				continue
			}

			counters.observed++

			e.queue = append(e.queue, &explainCandidate{
				queryid:     row.queryid,
				userid:      key.userid,
				dbid:        key.dbid,
				firstSeen:   s.Index,
				genericText: statementQueryText(row.query),
				textReason:  reasonTextTruncated,
			})
		}
	}

	n := min(DefaultMaxExplains, len(e.queue))

	attempt := e.queue[:n:n]
	e.queue = e.queue[n:]

	for _, c := range attempt {
		c.mode = planModeNone
	}

	counters.written = n
	counters.queued = len(e.queue)

	selection.candidates = attempt

	return selection
}

// eligible applies the three unexplainable classes plus self-exclusion, once, when a
// shape is first seen: a shape that cannot be planned from this connection is never
// queued. The masked class is the caller's, since a masked row has no identity to
// remember.
func eligible(row statementRow, s SampleContext, facts explainFacts, counters *explainCounters) bool {
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

// agentTruncated reports the cap+1 sentinel the statements read asks the server for:
// one rune past DefaultMaxQueryText means the agent cut it, so the prefix ends
// mid-token.
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

// --- the facts read ----------------------------------------------------------

type explainFacts struct {
	read bool

	searchPath         string
	selfOID            string
	autoExplainVisible bool

	// parameterCap is log_parameter_max_length in bytes: -1 unbounded, 0 off, else a
	// finite cap. parameterCapRead is false where the setting was absent.
	parameterCap     int64
	parameterCapRead bool
}

// readExplainFacts takes the one read, under the ordinary statement bound. The error
// is returned, not swallowed: a failure costs the selection its self-exclusion, the
// literal tier its gate and the summary its facts, which would otherwise read as an
// unbounded cap or an idle database.
func readExplainFacts(ctx context.Context, q RowQuerier) (explainFacts, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var (
		searchPath   *string
		selfOID      *string
		visible      *bool
		parameterCap *int64
	)

	err := q.QueryRow(stmtCtx, explainFactsSQL).Scan(&searchPath, &selfOID, &visible, &parameterCap)
	if err != nil {
		return explainFacts{}, err
	}

	return explainFacts{
		read:               true,
		searchPath:         text(searchPath),
		selfOID:            text(selfOID),
		autoExplainVisible: visible != nil && *visible,
		parameterCap:       int64Value(parameterCap),
		parameterCapRead:   parameterCap != nil,
	}, nil
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

// submitOne picks the mode from the evidence that is available, not from a rule, and
// sends it down the one prepared-statement path with that mode's setting and
// arguments.
func (e *Explain) submitOne(
	ctx context.Context, q RowQuerier, s SampleContext, c *explainCandidate,
) {
	mode, ok := choosePlanMode(c)
	if !ok {
		c.reason = mode

		return
	}

	e.prepared++
	c.prepared = true

	name := preparedStatementPrefix + strconv.Itoa(e.prepared)

	var (
		plan      []byte
		truncated bool
		err       error
	)

	if mode == planModeEstimatedLiteral {
		arguments := make([]string, len(c.literal.values))
		for i, v := range c.literal.values {
			arguments[i] = renderArgument(v)
		}

		c.parameters = len(arguments)

		plan, truncated, err = submitPreparedPlan(ctx, q, name, c.literal.query,
			forceCustomPlanSQL, arguments)
	} else {
		c.parameters = inferredParameters(c.genericText)

		plan, truncated, err = submitPreparedPlan(ctx, q, name, c.genericText,
			forceGenericPlanSQL, nullArguments(c.parameters))
	}

	if err != nil {
		c.err = s.errorText(err)

		return
	}

	c.mode = mode
	c.plan = plan
	c.planTruncated = truncated
	c.planQueryID = planQueryIdentifier(plan)

	// Only the literal mode can assert equality: its text is the one the server logged
	// for this identifier, where the generic text jumbles a Param for every constant
	// the original jumbled as a Const, so its identifier differs by construction.
	if mode == planModeEstimatedLiteral && c.planQueryID != "" && c.queryid != nil {
		c.queryIDMatch = strconv.FormatBool(c.planQueryID == strconv.FormatInt(*c.queryid, 10))
	}
}

// choosePlanMode returns the mode, or false with a reason in its place when nothing can
// be submitted. A bind record whose text cannot be used does not end the candidate: the
// generic mode is still tried. No version enters the choice: both tiers are a prepared
// statement, which every supported server has.
func choosePlanMode(c *explainCandidate) (mode string, ok bool) {
	if c.literal != nil {
		if !multiStatement(c.literal.query) {
			return planModeEstimatedLiteral, true
		}

		c.literalReason = reasonMultiStatement
	}

	switch {
	case c.genericText == "":
		return c.textReason, false

	case multiStatement(c.genericText):
		return reasonMultiStatement, false
	}

	return planModeEstimatedGeneric, true
}

// multiStatement reports whether text carries more than one command, ignoring a trailing
// separator. Deliberately crude: a ';' inside a string literal costs one candidate a plan,
// while a false negative would hand the simple protocol a batch to execute. Neither
// source should ever produce one - pg_stat_statements records each statement of a batch
// as its own row (measured on 14 and 18), and the extended protocol refuses a batch at
// Parse - so this is the guard behind the guards.
func multiStatement(query string) bool {
	return strings.Contains(strings.TrimRight(strings.TrimSpace(query), ";"), ";")
}

// explainOptions is the only place an option is added, and ANALYZE is not in it: it
// executes the statement, which here is the most expensive query on the server.
// default_transaction_read_only is not the control - measured, it stops EXPLAIN ANALYZE
// UPDATE and lets EXPLAIN ANALYZE SELECT count(*) run. SETTINGS is what makes the
// estimated tiers' blocks self-describing: the forced plan_cache_mode is printed in them.
func explainOptions() []string {
	return []string{"VERBOSE", "SETTINGS"}
}

func explainStatement(options []string, query string) string {
	return "EXPLAIN (" + strings.Join(options, ", ") + ") " + query
}

// The estimated tiers' statements around one candidate. The name is the agent's own
// and the text - normalized in the generic tier, the log's own in the literal - is
// spliced once, into PREPARE; the EXECUTE carries only the arguments, which are NULLs
// in the generic tier and the parser's decoded values in the literal, never a piece of
// the log's DETAIL text.
func prepareStatement(name, query string) string {
	return "PREPARE " + name + " AS " + query
}

func executeStatement(name string, arguments []string) string {
	if len(arguments) == 0 {
		// EXECUTE name() is a syntax error: the list, when present, is non-empty.
		return "EXECUTE " + name
	}

	return "EXECUTE " + name + "(" + strings.Join(arguments, ", ") + ")"
}

// nullArguments is the generic tier's list: one NULL per inferred parameter.
func nullArguments(parameters int) []string {
	arguments := make([]string, parameters)
	for i := range arguments {
		arguments[i] = "NULL"
	}

	return arguments
}

func deallocateStatement(name string) string {
	return "DEALLOCATE " + name
}

// simpleQuerier is the raw simple query protocol, an optional interface so the shared
// RowQuerier does not grow a method only this collector uses.
type simpleQuerier interface {
	ExecSimple(ctx context.Context, sql string, maxBytes int) ([]string, bool, error)
}

// errNoSimpleProtocol is a wiring failure, not a server one: the estimated modes cannot
// be submitted over anything else.
var errNoSimpleProtocol = errors.New("the estimated plan modes need the simple query protocol")

// submitPreparedPlan is both estimated tiers, one prepared-statement path on every
// version: PREPARE the text, force plan_cache_mode to the tier's setting, EXPLAIN
// EXECUTE with the tier's arguments, then restore the setting and DEALLOCATE on every
// exit - the failed ones included, in the reverse of the order they were made. Without
// ANALYZE, EXPLAIN EXECUTE plans the statement and does not run it.
//
// All five go over the raw simple query protocol. None carries a bind of its own, and
// the extended protocol is the wrong tool twice over: an unbound $1 fails at Bind
// before the server sees the statement, and pgx's QueryExecModeSimpleProtocol
// substitutes $n client-side and rejects it with "insufficient arguments" before the
// wire. The return is the server's bytes verbatim, one plan line per row, bounded at
// MaxPlanBytes; the second is what the block records as plan_truncated=.
func submitPreparedPlan(
	ctx context.Context, q RowQuerier, name, query, setting string, arguments []string,
) (plan []byte, truncated bool, err error) {
	simple, ok := q.(simpleQuerier)
	if !ok {
		return nil, false, errNoSimpleProtocol
	}

	if err := execSimple(ctx, simple, prepareStatement(name, query)); err != nil {
		return nil, false, err
	}

	// Best effort, as the RESET of statement_timeout is: a cleanup that fails here
	// fails because the connection is gone, and the next candidate's error says so.
	defer func() { _ = execSimple(ctx, simple, deallocateStatement(name)) }()

	if err := execSimple(ctx, simple, setting); err != nil {
		return nil, false, err
	}

	defer func() { _ = execSimple(ctx, simple, resetPlanCacheModeSQL) }()

	statement := explainStatement(explainOptions(), executeStatement(name, arguments))

	lines, truncated, err := simple.ExecSimple(ctx, statement, MaxPlanBytes)
	if err != nil {
		return nil, false, err
	}

	return planBytes(lines), truncated, nil
}

// execSimple runs one statement that returns nothing worth keeping.
func execSimple(ctx context.Context, simple simpleQuerier, sql string) error {
	_, _, err := simple.ExecSimple(ctx, sql, 0)

	return err
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
	w io.Writer, s SampleContext, c *explainCandidate, facts explainFacts,
) error {
	fields := []headerField{
		{"sample", strconv.Itoa(s.Index)},
		{"queryid", int64Text(c.queryid)},
		{"first_seen", strconv.Itoa(c.firstSeen)},
		{"mode", c.mode},
	}

	if c.prepared {
		fields = append(fields, headerField{"parameters", strconv.Itoa(c.parameters)})
	}

	if c.literalReason != "" {
		fields = append(fields, headerField{"literal_reason", c.literalReason})
	}

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

	if c.bindsSeen > 1 {
		fields = append(fields, headerField{"binds_seen", strconv.Itoa(c.bindsSeen)})
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
		// Mode logged attempts candidates but submits nothing, so one with no
		// attachable log entry has no plan.
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

		// Reached only from the closing pass: no sample's report claimed it, so there
		// was nothing to attach this to - which is not the same as having no identifier.
		reason = reasonNoRankedReport
	}

	fields = append(fields, headerField{"reason", reason})

	return e.writeBlock(w, s, fields, plan.body)
}

// writeSummary is the collector's own administrative block, one per sample: the
// window's field set is fixed and has no hook for one, as with the log tails' drain
// blocks.
func (e *Explain) writeSummary(
	w io.Writer, s SampleContext, selection explainSelection, facts explainFacts,
	gate string, read *tailRead, factsErr string,
) error {
	fields := []headerField{{"summary", "true"}}

	if selection.drain {
		fields = append(fields, headerField{"drain", "true"})
	} else {
		fields = append(fields, headerField{"sample", strconv.Itoa(s.Index)})
	}

	if len(selection.candidates) == 0 {
		fields = append(fields, headerField{"reason", reasonNoCandidates})
	}

	if selection.feedReason != "" {
		fields = append(fields, headerField{"statements_reason", selection.feedReason})
	}

	// The tier's gate, on the sample rather than only on its candidates, so a quiet
	// sample still says the literal tier was off and why.
	if e.submits() && !selection.drain && gate != "" {
		fields = append(fields, headerField{"literal_reason", gate})
	}

	fields = append(fields, selection.counters.fields()...)

	if e.store.ambiguous > 0 {
		fields = append(fields,
			headerField{"plans_ambiguous", strconv.Itoa(e.store.ambiguous)})
	}

	fields = append(fields, e.logFields(read)...)

	if facts.read {
		fields = append(fields,
			headerField{"auto_explain_visible", strconv.FormatBool(facts.autoExplainVisible)})
	}

	// Last, and only on failure: without it a read that never happened is
	// indistinguishable from an idle database.
	if factsErr != "" {
		fields = append(fields, headerField{"facts_error", factsErr})
	}

	return e.writeBlock(w, s, fields, nil)
}

// logFields reports the two stores in the log-tail engine's own vocabulary. A tail that
// never resolved gets its reason and log access and no count, as writeReasonBlock does:
// a zero beside a reason renders an absence as a measurement. The reason rides
// log_reason= because the selection already owns reason=.
func (e *Explain) logFields(read *tailRead) []headerField {
	source := e.tail.source
	store := e.store

	if source.reason != "" {
		return []headerField{
			{"log_reason", source.reason},
			{"log_access", source.logAccess()},
		}
	}

	fields := []headerField{
		{"log_access", source.logAccess()},
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

	// The bind records, under mode all only: mode logged submits nothing, so the count
	// would describe evidence it has no tier for. binds_harvested= is a measurement
	// like plans_harvested=; the rest separate "none in the log" from "none usable" -
	// no identifier to join by, refused by the parser, or bounded by the caps.
	if e.submits() {
		binds := e.binds

		fields = append(fields, headerField{"binds_harvested", strconv.Itoa(binds.total)})

		for _, f := range []struct {
			key   string
			value int
		}{
			{"binds_unidentified", binds.unidentified},
			{"binds_rejected", binds.rejected},
			{"binds_dropped", binds.dropped},
		} {
			if f.value > 0 {
				fields = append(fields, headerField{f.key, strconv.Itoa(f.value)})
			}
		}
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

func int64Value(v *int64) int64 {
	if v == nil {
		return 0
	}

	return *v
}

// explainMatch selects the two kinds of evidence the tiers read: auto_explain's plan
// entries, whose first line ends "plan:", and log_min_duration_statement's execute
// records, whose first line carries "execute" after the duration - and nothing else
// that opens "duration: <n> ms ", which is every statement the threshold logs. 00000 is
// registered paired because it is every LOG line in the file: without it the SQLSTATE
// match passes everything on csvlog and jsonlog, and the message predicate never runs.
var explainMatch = eventMatch{
	sqlstate:        []string{"00000"},
	paired:          []string{"00000"},
	message:         []string{"duration: "},
	messageSuffix:   []string{"plan:"},
	messageContains: []string{" ms  execute "},
}
