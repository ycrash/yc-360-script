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

// pid joins samples across ticks (application_name isn't unique per replica);
// it doesn't survive a WAL sender reconnect, so application_name/client_addr ride along too.
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

// Columns present in every supported PostgreSQL version.
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

// Columns pg_replication_slots gained after 14 (16/17/18). Read via to_jsonb
// since a bare SELECT would raise 42703 on servers that lack them.
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

// slices.Concat, not append: append would alias stableSlotColumns' backing array.
var slotColumns = slices.Concat(stableSlotColumns, optionalSlotColumnNames())

// Column-identical on PG 14-18. usesysid casts to text since pgx can't scan oid into
// *int32 mid-scan; client_addr uses host() to avoid /32 CIDR rendering. Ordered by pid, never a lag column, uncapped since max_wal_senders bounds it.
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

// Shared by every version; casts datoid, xmin, catalog_xmin and the two LSN
// columns to text for sendersSQL's pgx scan-plan reason.
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

// Disambiguates NULL-because-absent from NULL-because-not-applicable (e.g.
// conflicting on PG14 vs. on a physical slot), which an empty cell alone can't.
const optionalColumnsProbe = `       (SELECT string_agg(attname, ',' ORDER BY attname)
          FROM pg_catalog.pg_attribute
         WHERE attrelid = to_regclass('pg_catalog.pg_replication_slots')
           AND attname = ANY($1::text[])
           AND NOT attisdropped) AS optional_columns`

const slotsFrom = `
FROM pg_catalog.pg_replication_slots s
ORDER BY s.slot_name`

// Uncapped: max_replication_slots bounds the row count, so building a jsonb of
// each row six times costs nothing.
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

// Replication captures connected WAL senders and retaining slots.
// pg_stat_replication masks every column past application_name to NULL without
// pg_monitor, so a healthy and an hour-behind replica look byte-identical here.
type Replication struct {
	// Interval is the cadence, one run's DefaultInterval. Zero is the bookend alone.
	Interval time.Duration
}

func (r Replication) Artifact() Artifact {
	return Artifact{
		Name:     "pg_replication",
		FileName: "pg_replication.txt",
		Scope:    "cluster",
		Schedule: Periodic(r.Interval),

		// No SampleBudget: two statements is DefaultSampleBudget already, which is
		// what Periodic's closing sample contributes to moduleDeadline.
	}
}

// Sample reads pg_stat_replication and pg_replication_slots into one block each.
// A returned error means the write failed - each block already turns its own
// read failure into an error= header instead.
func (r Replication) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// Buffered so a mid-write failure can't leave a half-written sample.
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

// optional_columns= appears only when the probe returned a value (NULL on PG
// 14/15, absent when the slots view has no rows).
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

// senderRow is one connected WAL sender. pid is non-pointer since a NULL there
// should fail the statement, not produce an unjoinable row; every other field
// is a pointer because pg_monitor masking nulls them, and a masked row should
// render as empty cells rather than fail the block.
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

// slotRow: pointer fields' NULLs are meaningful - plugin/datoid/database/
// confirmed_flush_lsn are NULL by definition for a physical slot; safe_wal_size is NULL cluster-wide while max_slot_wal_keep_size sits at its -1 default.
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

	// Matches optionalSlotColumns' order; optional_columns= in the header
	// disambiguates NULL-because-absent from NULL-because-not-applicable.
	conflicting        *bool
	failover           *bool
	inactiveSince      *time.Time
	invalidationReason *string
	synced             *bool
	twoPhaseAt         *string
}

// readSlots returns the rows and the presence set for the block header; the
// latter is nil if the server has none of the optional columns or there are no rows.
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
			// timeText, not raw text: the ::timestamptz cast avoids jsonb's +00:00 offset form.
			timeText(row.inactiveSince),
			text(row.invalidationReason),
			boolText(row.synced),
			text(row.twoPhaseAt),
		}
	}

	return cells
}
