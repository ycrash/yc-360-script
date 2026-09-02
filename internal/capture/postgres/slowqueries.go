package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxStatements bounds one sample's statement rows;
// matches pg_stat_statements.max's own default, so truncated=true only when an operator raised the GUC (Grand Unified Configuration).
const DefaultMaxStatements = 5000

// The ways a block can be empty without a read having failed; each writes a
// header-only block with reason= (not error=).
const (
	// No pg_extension row: CREATE EXTENSION was never run in this database.
	reasonExtensionAbsent = "extension_absent"

	// Installed, but the bare name doesn't resolve to its view: the schema is off
	// search_path, carries no USAGE (schema_usage= says which), or is shadowed by a
	// same-named relation.
	reasonNotInSearchPath = "not_in_search_path"

	// Below extension 1.8, which is where total_time became total_exec_time.
	reasonExtensionTooOld = "extension_too_old"

	// Not in shared_preload_libraries: the view exists, and reading it raises 55000.
	reasonLibraryNotLoaded = "library_not_loaded"

	// Info view only, extension 1.8: it predates pg_stat_statements_info (added at
	// 1.9) while having total_exec_time, so the statements block still writes rows.
	// Never returned by reason(); the one reason the two blocks disagree on.
	reasonViewAbsent = "view_absent"
)

// statementColumnSpecs drives the select list, CSV header, preflight probe list and scan order together so they cannot drift apart.
var statementColumnSpecs = []struct {
	name     string
	expr     string // select-list expression; bare CTE column name for the 26 stable columns
	optional bool   // present only on some extension versions (1.8-1.12); read via to_jsonb(s) so a missing column yields NULL, not 42703
}{
	{"queryid", "queryid", false},
	{"userid", "userid", false},
	{"dbid", "dbid", false},
	{"toplevel", `(j ->> 'toplevel')::boolean`, true},
	{"plans", "plans", false},
	{"total_plan_time", "total_plan_time", false},
	{"min_plan_time", "min_plan_time", false},
	{"max_plan_time", "max_plan_time", false},
	{"calls", "calls", false},
	{"total_exec_time", "total_exec_time", false},
	{"min_exec_time", "min_exec_time", false},
	{"max_exec_time", "max_exec_time", false},
	{"rows", "rows", false},
	{"shared_blks_hit", "shared_blks_hit", false},
	{"shared_blks_read", "shared_blks_read", false},
	{"shared_blks_dirtied", "shared_blks_dirtied", false},
	{"shared_blks_written", "shared_blks_written", false},
	{"local_blks_hit", "local_blks_hit", false},
	{"local_blks_read", "local_blks_read", false},
	{"local_blks_dirtied", "local_blks_dirtied", false},
	{"local_blks_written", "local_blks_written", false},
	{"temp_blks_read", "temp_blks_read", false},
	{"temp_blks_written", "temp_blks_written", false},
	{"blk_read_time", `(j ->> 'blk_read_time')::float8`, true},
	{"blk_write_time", `(j ->> 'blk_write_time')::float8`, true},
	{"shared_blk_read_time", `(j ->> 'shared_blk_read_time')::float8`, true},
	{"shared_blk_write_time", `(j ->> 'shared_blk_write_time')::float8`, true},
	{"local_blk_read_time", `(j ->> 'local_blk_read_time')::float8`, true},
	{"local_blk_write_time", `(j ->> 'local_blk_write_time')::float8`, true},
	{"temp_blk_read_time", `(j ->> 'temp_blk_read_time')::float8`, true},
	{"temp_blk_write_time", `(j ->> 'temp_blk_write_time')::float8`, true},
	{"wal_records", "wal_records", false},
	{"wal_fpi", "wal_fpi", false},
	{"wal_bytes", "wal_bytes", false},
	{"stats_since", `(j ->> 'stats_since')::timestamptz`, true},
	{"minmax_stats_since", `(j ->> 'minmax_stats_since')::timestamptz`, true},
	{"query", "query", false},
}

// Derived from statementColumnSpecs: the CSV header and the preflight's $1.
var (
	statementColumns         = specNames(false)
	optionalStatementColumns = specNames(true)
)

func specNames(onlyOptional bool) []string {
	names := make([]string, 0, len(statementColumnSpecs))

	for _, spec := range statementColumnSpecs {
		if onlyOptional && !spec.optional {
			continue
		}

		names = append(names, spec.name)
	}

	return names
}

// statementKeyColumns is pg_stat_statements' real key (queryid alone is not unique) and the block's sort order.
var statementKeyColumns = []string{"queryid", "userid", "dbid", "toplevel"}

// specExpr panics at init if name isn't in statementColumnSpecs.
func specExpr(name string) string {
	for _, spec := range statementColumnSpecs {
		if spec.name == name {
			return spec.expr
		}
	}

	panic("statementColumnSpecs has no column " + name)
}

var infoColumns = []string{"dealloc", "stats_reset"}

// stats_reset moving invalidates every delta in the file as spanning a reset;
// dealloc rising means an evicted-and-reinserted row's counters restarted from zero.
// Below extension 1.11 a targeted per-row reset is undetectable by either column.
const infoSQL = `SELECT dealloc, stats_reset FROM pg_stat_statements_info`

// extensionSQL is the preflight: every object here is readable by a role holding nothing but LOGIN.
// meets_min_version probes pg_attribute for total_exec_time rather than parsing extversion, avoiding a "1.9" > "1.10" lexical-compare bug.
// visible compares the resolved OID against the extension's own view OID (not just to_regclass IS NOT NULL) so a same-named relation earlier in search_path can't pass as a match.
// library_loaded is read from pg_settings, not the catalog: CREATE EXTENSION can succeed while the library isn't preloaded, and the read then fails 55000.
const extensionSQL = `SELECT COALESCE((SELECT n.nspname::text
                   FROM pg_catalog.pg_extension e
                   JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
                  WHERE e.extname = 'pg_stat_statements'), '') AS schema,
       COALESCE((SELECT e.extversion
                   FROM pg_catalog.pg_extension e
                  WHERE e.extname = 'pg_stat_statements'), '') AS version,
       EXISTS (SELECT 1 FROM pg_catalog.pg_settings
                WHERE name = 'pg_stat_statements.max')          AS library_loaded,
       COALESCE(to_regclass('pg_stat_statements')::oid = (
           SELECT c.oid
             FROM pg_catalog.pg_class c
             JOIN pg_catalog.pg_extension e ON c.relnamespace = e.extnamespace
            WHERE e.extname = 'pg_stat_statements'
              AND c.relname = 'pg_stat_statements'), false)     AS visible,
       COALESCE((SELECT pg_catalog.has_schema_privilege(current_user, e.extnamespace, 'USAGE')
                   FROM pg_catalog.pg_extension e
                  WHERE e.extname = 'pg_stat_statements'), false) AS schema_usage,
       to_regclass('pg_stat_statements_info') IS NOT NULL       AS has_info,
       EXISTS (
           SELECT 1
             FROM pg_catalog.pg_attribute
            WHERE attrelid = to_regclass('pg_stat_statements')
              AND attname = 'total_exec_time'
              AND NOT attisdropped
       )                                                        AS meets_min_version,
       (SELECT string_agg(attname, ',' ORDER BY attname)
          FROM pg_catalog.pg_attribute
         WHERE attrelid = to_regclass('pg_stat_statements')
           AND attname = ANY($1::text[])
           AND NOT attisdropped)                                AS optional_columns`

// statementsCTE is the statement's fixed half; its own column order is inert since the outer select list references columns by name.
// to_jsonb(s) is computed once per row via MATERIALIZED: PostgreSQL doesn't CSE the eleven inline ->> extractions, so without this each is a separate to_jsonb build (measured ~7x slower on the 18 container's ~290-row view).
// count(*) OVER () sits inside the CTE, evaluated before LIMIT, giving the uncapped total behind statements_total=/truncated=.
// left(s.query, $2) is the only bound on query text: pg_stat_statements ignores track_activity_query_size entirely (measured: an 8189-char query stored under a 1kB setting of that GUC).
// 'query' is excluded from to_jsonb(s) (- 'query'): the MATERIALIZED CTE buffers rows in a tuplestore, and the untruncated query text there could exceed work_mem and spill to temp files, perturbing pg_health.txt's temp_files/temp_bytes counters.
// wal_bytes is cast to text: numeric is unbounded and a fractional value (not guaranteed absent) would break an int64 scan; userid/dbid are oid, cast to text for the same scan-safety reason.
const statementsCTE = `WITH m AS MATERIALIZED (
    SELECT s.queryid,
           s.userid::text AS userid,
           s.dbid::text   AS dbid,
           s.plans, s.total_plan_time, s.min_plan_time, s.max_plan_time,
           s.calls, s.total_exec_time, s.min_exec_time, s.max_exec_time, s.rows,
           s.shared_blks_hit, s.shared_blks_read, s.shared_blks_dirtied, s.shared_blks_written,
           s.local_blks_hit, s.local_blks_read, s.local_blks_dirtied, s.local_blks_written,
           s.temp_blks_read, s.temp_blks_written,
           s.wal_records, s.wal_fpi,
           s.wal_bytes::text     AS wal_bytes,
           left(s.query, $2)     AS query,
           to_jsonb(s) - 'query' AS j,
           count(*) OVER ()      AS statements_total
      FROM pg_stat_statements s
)`

// statementsSQL is compiled once at init from statementColumnSpecs and pinned by golden tests, not rebuilt per query from a live pg_attribute probe.
var statementsSQL = buildStatementsSQL()

// buildStatementsSQL renders the outer select list and ORDER BY from statementColumnSpecs.
// ORDER BY is on identity (queryid, userid, dbid, toplevel), never a statistic, so both endpoints of the window share a stable, mergeable ordering; a top-N by total_exec_time could select different sets at each endpoint and break the delta.
// Masked rows (queryid NULL) tie on the remaining columns and sort last (NULLS LAST default), so a binding cap sheds unattributable rows first.
func buildStatementsSQL() string {
	var sql strings.Builder

	sql.WriteString(statementsCTE)
	sql.WriteString("\nSELECT ")

	for i, spec := range statementColumnSpecs {
		if i > 0 {
			sql.WriteString(",\n       ")
		}

		sql.WriteString(spec.expr)

		// Aliased so an engineer running this by hand out of the bundle gets named columns instead of four columns called float8.
		if spec.expr != spec.name {
			sql.WriteString(" AS ")
			sql.WriteString(spec.name)
		}
	}

	sql.WriteString(",\n       statements_total\n  FROM m\n ORDER BY ")

	for i, name := range statementKeyColumns {
		if i > 0 {
			sql.WriteString(", ")
		}

		sql.WriteString(specExpr(name))
	}

	sql.WriteString("\n LIMIT $1")

	return sql.String()
}

// SlowQueries captures pg_stat_statements every sample; the delta, ranking, and top-N are the server's, not the agent's.
// The view needs no grant, but a role without pg_read_all_stats gets queryid NULL and query masked on rows it doesn't own while every counter stays exact (measured: 234/270 rows on 18, 124/152 on 14) - the inverse of pg_sessions.txt, where identity survives and detail doesn't. pg_metadata.txt's has_pg_read_all_stats is the bundle's only signal of this.
type SlowQueries struct {
	// Interval is the cadence, one run's frequency. Zero is the bookend alone.
	Interval time.Duration

	// MaxStatements bounds one sample's statement rows. Zero takes DefaultMaxStatements.
	MaxStatements int

	// feeds holds each sample's read until Explain takes it on the same tick, keyed by
	// sample so a read is offered to its own sample and no other. Never written to any
	// file by this collector: it is the feed the explain selection walks, not a
	// measurement.
	feeds map[int]statementFeed
}

// NewSlowQueries constructs the collector. A pointer because Sample retains across
// samples, and a field assigned through a value receiver would be assigned to a copy.
func NewSlowQueries() *SlowQueries { return &SlowQueries{} }

// statementKey is pg_stat_statements' real key; queryid alone is not unique across users
// or databases. toplevel is a string tri-state so NULL - every row below extension 1.9 -
// is a key value rather than a wildcard, as the server's own merge treats it.
type statementKey struct {
	queryid  int64
	userid   string
	dbid     string
	toplevel string
}

// statementFeed is one sample's read, offered to Explain on the same tick and taken
// once: the rows carry the normalized text the generic mode submits, and a read that
// yielded nothing says why, so the explain summary can repeat the reason rather than
// report an idle database.
type statementFeed struct {
	sample    int
	rows      []statementRow
	read      bool
	truncated bool

	// reason is why there are no rows: the extension's own reason, or the read's error
	// text, redacted.
	reason string
}

// feed hands Explain the read taken for this sample, once - the one coupling between
// the two collectors. False means no read was offered for the sample, so a tick that
// never reached the view is not walked as if it had.
func (sq *SlowQueries) feed(s SampleContext) (statementFeed, bool) {
	feed, ok := sq.feeds[s.Index]
	if ok {
		delete(sq.feeds, s.Index)
	}

	return feed, ok
}

func (sq *SlowQueries) offer(feed statementFeed) {
	if sq.feeds == nil {
		sq.feeds = map[int]statementFeed{}
	}

	sq.feeds[feed.sample] = feed
}

func statementRowKey(row statementRow) (statementKey, bool) {
	if row.queryid == nil {
		return statementKey{}, false
	}

	return statementKey{
		queryid:  *row.queryid,
		userid:   text(row.userid),
		dbid:     text(row.dbid),
		toplevel: boolKey(row.toplevel),
	}, true
}

// boolKey renders a tri-state pointer as a comparable key component; "" is NULL.
func boolKey(b *bool) string {
	if b == nil {
		return ""
	}

	return strconv.FormatBool(*b)
}

func (sq *SlowQueries) Artifact() Artifact {
	return Artifact{
		Name:     "pg_slow_queries",
		FileName: "pg_slow_queries.txt",

		// cluster, not database: pg_stat_statements holds stats for every database in the cluster, tagged by dbid, though the extension is installed per database.
		Scope: "cluster",

		Schedule: Periodic(sq.Interval),

		// Preflight + two reads on every sample: 3x StatementTimeout. Periodic's last sample is the close, so moduleDeadline sums this against Capacity and Bloat.
		SampleBudget: 3 * StatementTimeout,
	}
}

// Sample runs the preflight once and passes its result to both blocks so they can't disagree about what the server has.
// Info reads bracket the window (first on the opening sample, last on every later one) so the first and last stats_reset readings enclose everything between the endpoints.
// Errors returned here are write failures; read errors are captured as error= header fields instead.
func (sq *SlowQueries) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	ext, extErr := readExtension(ctx, q)

	var sample bytes.Buffer

	// Total == 1 (single-sample window) and every sample after the first take the closing (infoFirst=false) order.
	infoFirst := s.Index == 1 && s.Total > 1

	if infoFirst {
		if err := sq.writeInfoBlock(ctx, q, &sample, s, ext, extErr); err != nil {
			return err
		}
	}

	if err := sq.writeStatementsBlock(ctx, q, &sample, s, ext, extErr); err != nil {
		return err
	}

	if !infoFirst {
		if err := sq.writeInfoBlock(ctx, q, &sample, s, ext, extErr); err != nil {
			return err
		}
	}

	_, writeErr := w.Write(sample.Bytes())

	return writeErr
}

// writeStatementsBlock writes the block header and, when the preflight cleared the way, the rows.
// Fact keys (library_loaded, schema_usage, optional_columns) are dropped rather than written false/zero on failure,
// since an invented value would assert something nobody read;
// library_loaded rides every capture so a "never created + never preloaded" combination stays visible, schema_usage rides only with reason=not_in_search_path,
// and optional_columns rides wherever the view was readable even if the read then failed.
// statements_total/truncated are likewise dropped, not zeroed, on a failed read.
func (sq *SlowQueries) writeStatementsBlock(ctx context.Context, q RowQuerier, w io.Writer,
	s SampleContext, ext extensionFacts, extErr error,
) error {
	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if extErr != nil {
		sq.retainReason(s, s.errorText(extErr))

		return sq.writeStatements(w, append(fields, headerField{"error", s.errorText(extErr)}), s.At, nil)
	}

	fields = append(fields,
		headerField{"extension_version", ext.version},
		headerField{"extension_schema", ext.schema},
		headerField{"library_loaded", strconv.FormatBool(ext.libraryLoaded)},
	)

	if reason := ext.reason(); reason != "" {
		if reason == reasonNotInSearchPath {
			fields = append(fields, headerField{"schema_usage", strconv.FormatBool(ext.schemaUsage)})
		}

		sq.retainReason(s, reason)

		return sq.writeStatements(w, append(fields, headerField{"reason", reason}), s.At, nil)
	}

	rows, total, err := sq.readStatements(ctx, q)

	cells, queriesTruncated := statementCells(rows)

	if err == nil {
		sq.retain(s, rows, int64(len(rows)) < total)
	} else {
		sq.retainReason(s, s.errorText(err))
	}

	if ext.optionalColumns != nil {
		fields = append(fields, headerField{"optional_columns", *ext.optionalColumns})
	}

	if err != nil {
		return sq.writeStatements(w, append(fields, headerField{"error", s.errorText(err)}), s.At, nil)
	}

	fields = append(fields,
		headerField{"statements_written", strconv.Itoa(len(rows))},
		headerField{"statements_total", strconv.FormatInt(total, 10)},
		headerField{"truncated", strconv.FormatBool(int64(len(rows)) < total)},
	)

	if queriesTruncated > 0 {
		fields = append(fields, headerField{"queries_truncated", strconv.Itoa(queriesTruncated)})
	}

	return sq.writeStatements(w, fields, s.At, cells)
}

func (sq *SlowQueries) writeStatements(w io.Writer, fields []headerField, at time.Time, cells [][]string) error {
	if err := writeBlockHeader(w, "pg_stat_statements", sq.Artifact().Scope, fields, at); err != nil {
		return err
	}

	return writeRows(w, statementColumns, cells)
}

// writeInfoBlock mirrors the statements block's reason= wherever the cause is shared,
// and adds reasonViewAbsent, which is its alone.
func (sq *SlowQueries) writeInfoBlock(ctx context.Context, q RowQuerier, w io.Writer,
	s SampleContext, ext extensionFacts, extErr error,
) error {
	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if extErr != nil {
		return sq.writeInfo(w, append(fields, headerField{"error", s.errorText(extErr)}), s.At, nil)
	}

	fields = append(fields, headerField{"extension_version", ext.version})

	if reason := ext.reason(); reason != "" {
		return sq.writeInfo(w, append(fields, headerField{"reason", reason}), s.At, nil)
	}

	if !ext.hasInfo {
		return sq.writeInfo(w, append(fields, headerField{"reason", reasonViewAbsent}), s.At, nil)
	}

	row, err := readInfo(ctx, q)
	if err != nil {
		return sq.writeInfo(w, append(fields, headerField{"error", s.errorText(err)}), s.At, nil)
	}

	return sq.writeInfo(w, fields, s.At, infoCells(row))
}

// retain offers every sample's read to Explain, which walks it for shapes it has not
// seen: with no delta to compute, a middle sample is as good a feed as an endpoint.
func (sq *SlowQueries) retain(s SampleContext, rows []statementRow, truncated bool) {
	sq.offer(statementFeed{sample: s.Index, rows: rows, read: true, truncated: truncated})
}

// retainReason offers the reason there were no rows, so Explain's summary can say
// statements_reason= rather than reporting an idle database.
func (sq *SlowQueries) retainReason(s SampleContext, reason string) {
	sq.offer(statementFeed{sample: s.Index, reason: reason})
}

func (sq *SlowQueries) writeInfo(w io.Writer, fields []headerField, at time.Time, cells [][]string) error {
	if err := writeBlockHeader(w, "pg_stat_statements_info", sq.Artifact().Scope, fields, at); err != nil {
		return err
	}

	return writeRows(w, infoColumns, cells)
}

// infoRow is pg_stat_statements_info's single row; stats_reset is NULL when the cluster's statistics have never been reset.
type infoRow struct {
	dealloc    *int64
	statsReset *time.Time
}

func readInfo(ctx context.Context, q RowQuerier) (*infoRow, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var row infoRow

	if err := q.QueryRow(stmtCtx, infoSQL).Scan(&row.dealloc, &row.statsReset); err != nil {
		return nil, err
	}

	return &row, nil
}

func infoCells(row *infoRow) [][]string {
	if row == nil {
		return nil
	}

	return [][]string{{int64Text(row.dealloc), timeText(row.statsReset)}}
}

// statementRow is one pg_stat_statements entry. queryid is a pointer despite being the join key:
// a role without pg_read_all_stats gets queryid NULL (and query masked) on rows it doesn't own
// while every counter stays exact - a non-pointer would cost the entire block on a least-privilege capture.
// Every other field is a pointer too;
// the eleven optional ones are NULL both when the server lacks the column and when it has an unset value,
// disambiguated by optional_columns= in the header.
type statementRow struct {
	queryid *int64
	userid  *string
	dbid    *string

	toplevel *bool

	plans         *int64
	totalPlanTime *float64
	minPlanTime   *float64
	maxPlanTime   *float64

	calls         *int64
	totalExecTime *float64
	minExecTime   *float64
	maxExecTime   *float64
	rows          *int64

	sharedBlksHit     *int64
	sharedBlksRead    *int64
	sharedBlksDirtied *int64
	sharedBlksWritten *int64
	localBlksHit      *int64
	localBlksRead     *int64
	localBlksDirtied  *int64
	localBlksWritten  *int64
	tempBlksRead      *int64
	tempBlksWritten   *int64

	// Pre-1.11 and post-1.11 pairs are never both populated: extension 1.11 renamed and re-scoped them, splitting local buffer I/O out of the shared counters.
	blkReadTime        *float64
	blkWriteTime       *float64
	sharedBlkReadTime  *float64
	sharedBlkWriteTime *float64
	localBlkReadTime   *float64
	localBlkWriteTime  *float64
	tempBlkReadTime    *float64
	tempBlkWriteTime   *float64

	walRecords *int64
	walFPI     *int64
	walBytes   *string

	statsSince       *time.Time
	minmaxStatsSince *time.Time

	query *string
}

// dest is statementColumnSpecs' order restated by hand; checked against an independent SELECT by the matrix's anchor-row comparison.
func (r *statementRow) dest() []any {
	return []any{
		&r.queryid,
		&r.userid,
		&r.dbid,
		&r.toplevel,
		&r.plans,
		&r.totalPlanTime,
		&r.minPlanTime,
		&r.maxPlanTime,
		&r.calls,
		&r.totalExecTime,
		&r.minExecTime,
		&r.maxExecTime,
		&r.rows,
		&r.sharedBlksHit,
		&r.sharedBlksRead,
		&r.sharedBlksDirtied,
		&r.sharedBlksWritten,
		&r.localBlksHit,
		&r.localBlksRead,
		&r.localBlksDirtied,
		&r.localBlksWritten,
		&r.tempBlksRead,
		&r.tempBlksWritten,
		&r.blkReadTime,
		&r.blkWriteTime,
		&r.sharedBlkReadTime,
		&r.sharedBlkWriteTime,
		&r.localBlkReadTime,
		&r.localBlkWriteTime,
		&r.tempBlkReadTime,
		&r.tempBlkWriteTime,
		&r.walRecords,
		&r.walFPI,
		&r.walBytes,
		&r.statsSince,
		&r.minmaxStatsSince,
		&r.query,
	}
}

// readStatements returns the capped rows and the uncapped total.
func (sq *SlowQueries) readStatements(ctx context.Context, q RowQuerier) ([]statementRow, int64, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	// One rune past the cap: lets the render pass distinguish a cell the agent cut from one that arrived exactly at the limit.
	rows, err := q.Query(stmtCtx, statementsSQL, sq.maxStatements(), DefaultMaxQueryText+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var (
		collected []statementRow
		total     int64
	)

	for rows.Next() {
		var row statementRow

		if err := rows.Scan(append(row.dest(), &total)...); err != nil {
			return nil, 0, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return collected, total, nil
}

// statementCells renders rows and returns how many query cells the agent's own cap (not the server's) truncated.
func statementCells(rows []statementRow) ([][]string, int) {
	cells := make([][]string, len(rows))
	truncated := 0

	for i, row := range rows {
		query := text(row.query)

		if capped := truncateRunes(query, DefaultMaxQueryText); capped != query {
			query = capped
			truncated++
		}

		cells[i] = []string{
			int64Text(row.queryid),
			text(row.userid),
			text(row.dbid),
			boolText(row.toplevel),
			int64Text(row.plans),
			float64Text(row.totalPlanTime),
			float64Text(row.minPlanTime),
			float64Text(row.maxPlanTime),
			int64Text(row.calls),
			float64Text(row.totalExecTime),
			float64Text(row.minExecTime),
			float64Text(row.maxExecTime),
			int64Text(row.rows),
			int64Text(row.sharedBlksHit),
			int64Text(row.sharedBlksRead),
			int64Text(row.sharedBlksDirtied),
			int64Text(row.sharedBlksWritten),
			int64Text(row.localBlksHit),
			int64Text(row.localBlksRead),
			int64Text(row.localBlksDirtied),
			int64Text(row.localBlksWritten),
			int64Text(row.tempBlksRead),
			int64Text(row.tempBlksWritten),
			float64Text(row.blkReadTime),
			float64Text(row.blkWriteTime),
			float64Text(row.sharedBlkReadTime),
			float64Text(row.sharedBlkWriteTime),
			float64Text(row.localBlkReadTime),
			float64Text(row.localBlkWriteTime),
			float64Text(row.tempBlkReadTime),
			float64Text(row.tempBlkWriteTime),
			int64Text(row.walRecords),
			int64Text(row.walFPI),
			text(row.walBytes),
			// timeText, not the jsonb extraction's raw form: keeps timestamptz's artifact form instead of jsonb's +00:00 offset form.
			timeText(row.statsSince),
			timeText(row.minmaxStatsSince),
			query,
		}
	}

	return cells, truncated
}

// extensionFacts is extensionSQL's one row.
// optionalColumns is a pointer because string_agg over zero catalogue rows is NULL.
// The rest are non-pointers: every bool is an EXISTS/COALESCE and both strings are COALESCEd to the empty string,
// so a NULL there would mean a broken statement, not a server answer.
type extensionFacts struct {
	schema          string
	version         string
	libraryLoaded   bool
	visible         bool
	schemaUsage     bool
	hasInfo         bool
	meetsMinVersion bool
	optionalColumns *string
}

// reason returns which of the four absences applies, empty when the view can be read.
// Order is remedy order, not detection order: extension_absent takes priority
// even when the library is also unloaded, since CREATE EXTENSION must happen first.
func (e extensionFacts) reason() string {
	switch {
	case e.schema == "":
		return reasonExtensionAbsent

	case !e.visible:
		return reasonNotInSearchPath

	case !e.meetsMinVersion:
		return reasonExtensionTooOld

	case !e.libraryLoaded:
		return reasonLibraryNotLoaded
	}

	return ""
}

func readExtension(ctx context.Context, q RowQuerier) (extensionFacts, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	var facts extensionFacts

	if err := q.QueryRow(stmtCtx, extensionSQL, optionalStatementColumns).Scan(
		&facts.schema,
		&facts.version,
		&facts.libraryLoaded,
		&facts.visible,
		&facts.schemaUsage,
		&facts.hasInfo,
		&facts.meetsMinVersion,
		&facts.optionalColumns,
	); err != nil {
		return extensionFacts{}, err
	}

	return facts, nil
}

func (sq *SlowQueries) maxStatements() int {
	if sq.MaxStatements <= 0 {
		return DefaultMaxStatements
	}

	return sq.MaxStatements
}
