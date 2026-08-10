package postgres

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"
)

const DefaultReplicationInterval = 10 * time.Second

// pid leads: it is the key consecutive samples are stitched on, where
// application_name is not unique per replica. pid does not survive a WAL sender
// reconnect, so application_name and client_addr are captured beside it.
var replicationColumns = []string{
	"pid",
	"usesysid",
	"usename",
	"application_name",
	"client_addr",
	"client_hostname",
	"client_port",
	"backend_start",
	"backend_xmin",
	"state",
	"sent_lsn",
	"write_lsn",
	"flush_lsn",
	"replay_lsn",
	"write_lag_seconds",
	"flush_lag_seconds",
	"replay_lag_seconds",
	"sync_priority",
	"sync_state",
	"reply_time",
}

// stableSlotColumns are the pg_replication_slots columns every supported
// version has.
var stableSlotColumns = []string{
	"slot_name",
	"plugin",
	"slot_type",
	"datoid",
	"database",
	"temporary",
	"active",
	"active_pid",
	"xmin",
	"catalog_xmin",
	"restart_lsn",
	"confirmed_flush_lsn",
	"wal_status",
	"safe_wal_size",
	"two_phase",
}

// optionalSlotColumns are the pg_replication_slots columns added after 14 - one
// at 16, four at 17, one at 18. Read through to_jsonb because a server without
// the column yields NULL there where a bare SELECT raises 42703, so one
// statement covers every version instead of four variants and three capability
// flags. Name and expression are paired so $1, the select list and the column
// header cannot drift; adding a column is one entry.
var optionalSlotColumns = []struct {
	name string
	expr string
}{
	{"conflicting", `(to_jsonb(s) ->> 'conflicting')::boolean`},
	{"failover", `(to_jsonb(s) ->> 'failover')::boolean`},
	{"inactive_since", `(to_jsonb(s) ->> 'inactive_since')::timestamptz`},
	{"invalidation_reason", `to_jsonb(s) ->> 'invalidation_reason'`},
	{"synced", `(to_jsonb(s) ->> 'synced')::boolean`},
	{"two_phase_at", `to_jsonb(s) ->> 'two_phase_at'`},
}

func optionalSlotColumnNames() []string {
	names := make([]string, len(optionalSlotColumns))
	for i, column := range optionalSlotColumns {
		names[i] = column.name
	}

	return names
}

// slices.Concat rather than append: appending to stableSlotColumns would share
// its backing array.
var slotColumns = slices.Concat(stableSlotColumns, optionalSlotColumnNames())

// sendersSQL reads the connected WAL senders. The view is column-identical on 14
// through 18, so there is no variant and no capability flag.
//
// Every column typed outside int2/int4/int8, bool, text, float8 and timestamptz
// is cast here rather than mapped in the scan. usesysid is the load-bearing one:
// pgx has no scan plan from oid into a nullable *int32, and it sits mid-scan, so
// selecting it bare would cost the whole block. client_addr takes host() instead,
// since ::text renders 10.0.4.12/32.
//
// ORDER BY pid, never on a lag: ordering on a statistic hands the server a
// different row set per sample. Uncapped because max_wal_senders bounds the row
// source and sizes shared memory at startup.
const sendersSQL = `SELECT pid,
       usesysid::text,
       usename::text,
       application_name::text,
       host(client_addr),
       client_hostname::text,
       client_port,
       backend_start,
       backend_xmin::text,
       state::text,
       sent_lsn::text,
       write_lsn::text,
       flush_lsn::text,
       replay_lsn::text,
       EXTRACT(EPOCH FROM write_lag)::float8,
       EXTRACT(EPOCH FROM flush_lag)::float8,
       EXTRACT(EPOCH FROM replay_lag)::float8,
       sync_priority,
       sync_state::text,
       reply_time
FROM pg_catalog.pg_stat_replication
ORDER BY pid`

// stableSlotsSelect is the part of slotsSQL every supported version shares.
// sendersSQL's cast rule applies to datoid, xmin, catalog_xmin and the two
// pg_lsn columns.
const stableSlotsSelect = `SELECT s.slot_name::text,
       s.plugin::text,
       s.slot_type::text,
       s.datoid::text,
       s.database::text,
       s.temporary,
       s.active,
       s.active_pid,
       s.xmin::text,
       s.catalog_xmin::text,
       s.restart_lsn::text,
       s.confirmed_flush_lsn::text,
       s.wal_status::text,
       s.safe_wal_size,
       s.two_phase`

// optionalColumnsProbe is which of optionalSlotColumns the server has, carried
// per row as count(*) OVER () is elsewhere and landing in the block header. It
// answers a question about the server, not the row: conflicting is NULL on 14
// because the column is absent and NULL on 18 for a physical slot because it
// does not apply, and an empty cell cannot tell those apart. Lost when the view
// returns no rows, which is right - there are then no empty cells to separate.
const optionalColumnsProbe = `       (SELECT string_agg(attname, ',' ORDER BY attname)
          FROM pg_catalog.pg_attribute
         WHERE attrelid = to_regclass('pg_catalog.pg_replication_slots')
           AND attname = ANY($1::text[])
           AND NOT attisdropped) AS optional_columns`

const slotsFrom = `
FROM pg_catalog.pg_replication_slots s
ORDER BY s.slot_name`

// slotsSQL reads the slots, one statement for every version. Uncapped for
// sendersSQL's reason, the bound here being max_replication_slots - which is
// also why building a jsonb of the row six times per row costs nothing.
var slotsSQL = buildSlotsSQL()

func buildSlotsSQL() string {
	var sql strings.Builder

	sql.WriteString(stableSlotsSelect)

	for _, column := range optionalSlotColumns {
		sql.WriteString(",\n       ")
		sql.WriteString(column.expr)
	}

	sql.WriteString(",\n")
	sql.WriteString(optionalColumnsProbe)
	sql.WriteString(slotsFrom)

	return sql.String()
}

// Replication captures which replicas are connected and how far behind each is,
// and which slots are retaining WAL.
//
// pg_replication_slots needs no grant. pg_stat_replication returns its rows to
// any role but masks every column past application_name to NULL without
// pg_monitor, so a least-privilege capture of a healthy cluster and of one whose
// replica is an hour behind are byte-identical in the lag columns. Nothing here
// can tell them apart; pg_metadata.txt's has_pg_monitor_role can.
type Replication struct {
	// Interval is the cadence. Zero takes DefaultReplicationInterval.
	Interval time.Duration
}

func (r Replication) Artifact() Artifact {
	return Artifact{
		Name:     "pg_replication",
		FileName: "pg_replication.txt",
		Scope:    "cluster",
		Schedule: Every(r.interval()),

		// No SampleBudget: two statements is DefaultSampleBudget, and an
		// interval collector's offsets are strictly inside the window, so this
		// one never reaches the closing tick moduleDeadline sums budgets for.
	}
}

// Sample reads the two views and writes one block each. Each block turns its own
// read failure into an error= header and an empty body. An error from here means
// the write failed, not that either read did.
func (r Replication) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// One buffer, one Write: a write failing between the blocks would leave the
	// window's stub behind a half-written sample.
	var sample bytes.Buffer

	if err := r.writeSendersBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	if err := r.writeSlotsBlock(ctx, q, &sample, s); err != nil {
		return err
	}

	_, err := w.Write(sample.Bytes())

	return err
}

func (r Replication) writeSendersBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, err := readSenders(ctx, q)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	if err := writeBlockHeader(w, "pg_stat_replication", r.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	return writeRows(w, replicationColumns, senderCells(rows))
}

// writeSlotsBlock writes optional_columns= only when the probe returned a value:
// NULL on 14 and 15, and absent when the view returned no rows to carry it.
func (r Replication) writeSlotsBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, optionalColumns, err := readSlots(ctx, q)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	switch {
	case err != nil:
		fields = append(fields, headerField{"error", s.errorText(err)})

	case optionalColumns != nil:
		fields = append(fields, headerField{"optional_columns", *optionalColumns})
	}

	if err := writeBlockHeader(w, "pg_replication_slots", r.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	return writeRows(w, slotColumns, slotCells(rows))
}

// senderRow is one connected WAL sender. pid is the join key and a non-pointer,
// as bloatRow.relid and healthRow.datid are: a NULL there should cost the
// statement rather than write a row nothing can join. Safe because pid survives
// masking, where state and every LSN and lag column do not - which is why every
// other field is a pointer, so a masked row renders as empty cells rather than
// costing the block.
type senderRow struct {
	pid             int32
	usesysid        *string
	usename         *string
	applicationName *string
	clientAddr      *string
	clientHostname  *string
	clientPort      *int32
	backendStart    *time.Time
	backendXmin     *string
	state           *string
	sentLSN         *string
	writeLSN        *string
	flushLSN        *string
	replayLSN       *string
	writeLag        *float64
	flushLag        *float64
	replayLag       *float64
	syncPriority    *int32
	syncState       *string
	replyTime       *time.Time
}

func readSenders(ctx context.Context, q RowQuerier) ([]senderRow, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, sendersSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []senderRow

	for rows.Next() {
		var row senderRow

		if err := rows.Scan(
			&row.pid,
			&row.usesysid,
			&row.usename,
			&row.applicationName,
			&row.clientAddr,
			&row.clientHostname,
			&row.clientPort,
			&row.backendStart,
			&row.backendXmin,
			&row.state,
			&row.sentLSN,
			&row.writeLSN,
			&row.flushLSN,
			&row.replayLSN,
			&row.writeLag,
			&row.flushLag,
			&row.replayLag,
			&row.syncPriority,
			&row.syncState,
			&row.replyTime,
		); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

func senderCells(rows []senderRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			strconv.FormatInt(int64(row.pid), 10),
			text(row.usesysid),
			text(row.usename),
			text(row.applicationName),
			text(row.clientAddr),
			text(row.clientHostname),
			int32Text(row.clientPort),
			timeText(row.backendStart),
			text(row.backendXmin),
			text(row.state),
			text(row.sentLSN),
			text(row.writeLSN),
			text(row.flushLSN),
			text(row.replayLSN),
			float64Text(row.writeLag),
			float64Text(row.flushLag),
			float64Text(row.replayLag),
			int32Text(row.syncPriority),
			text(row.syncState),
			timeText(row.replyTime),
		}
	}

	return cells
}

// slotRow is one replication slot. slot_name is the join key and a non-pointer,
// for senderRow.pid's reason; every other field is a pointer because a slot's
// NULLs all mean something. plugin, datoid, database and confirmed_flush_lsn are
// NULL for a physical slot by definition; xmin and catalog_xmin for a slot
// holding no snapshot; restart_lsn and wal_status together for one that never
// reserved WAL; safe_wal_size for every slot in the cluster while
// max_slot_wal_keep_size is at its -1 default.
type slotRow struct {
	slotName          string
	plugin            *string
	slotType          *string
	datoid            *string
	database          *string
	temporary         *bool
	active            *bool
	activePID         *int32
	xmin              *string
	catalogXmin       *string
	restartLSN        *string
	confirmedFlushLSN *string
	walStatus         *string
	safeWALSize       *int64
	twoPhase          *bool

	// In optionalSlotColumns' order. Each is NULL both where the server lacks
	// the column and where it does not apply, which optional_columns= in the
	// header separates.
	conflicting        *bool
	failover           *bool
	inactiveSince      *time.Time
	invalidationReason *string
	synced             *bool
	twoPhaseAt         *string
}

// readSlots returns the rows and the presence set for the block header, nil
// where the server has none of the optional columns and where there are no rows.
func readSlots(ctx context.Context, q RowQuerier) ([]slotRow, *string, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, slotsSQL, optionalSlotColumnNames())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var (
		collected       []slotRow
		optionalColumns *string
	)

	for rows.Next() {
		var row slotRow

		if err := rows.Scan(
			&row.slotName,
			&row.plugin,
			&row.slotType,
			&row.datoid,
			&row.database,
			&row.temporary,
			&row.active,
			&row.activePID,
			&row.xmin,
			&row.catalogXmin,
			&row.restartLSN,
			&row.confirmedFlushLSN,
			&row.walStatus,
			&row.safeWALSize,
			&row.twoPhase,
			&row.conflicting,
			&row.failover,
			&row.inactiveSince,
			&row.invalidationReason,
			&row.synced,
			&row.twoPhaseAt,
			&optionalColumns,
		); err != nil {
			return nil, nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	return collected, optionalColumns, nil
}

func slotCells(rows []slotRow) [][]string {
	cells := make([][]string, len(rows))

	for i, row := range rows {
		cells[i] = []string{
			row.slotName,
			text(row.plugin),
			text(row.slotType),
			text(row.datoid),
			text(row.database),
			boolText(row.temporary),
			boolText(row.active),
			int32Text(row.activePID),
			text(row.xmin),
			text(row.catalogXmin),
			text(row.restartLSN),
			text(row.confirmedFlushLSN),
			text(row.walStatus),
			int64Text(row.safeWALSize),
			boolText(row.twoPhase),
			boolText(row.conflicting),
			boolText(row.failover),
			// Through timeText, not as the extraction rendered it: the
			// ::timestamptz cast is what keeps this in the artifact's timestamp
			// form rather than jsonb's +00:00 offset form.
			timeText(row.inactiveSince),
			text(row.invalidationReason),
			boolText(row.synced),
			text(row.twoPhaseAt),
		}
	}

	return cells
}

func (r Replication) interval() time.Duration {
	if r.Interval <= 0 {
		return DefaultReplicationInterval
	}

	return r.Interval
}
