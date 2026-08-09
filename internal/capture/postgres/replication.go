package postgres

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"time"
)

// DefaultReplicationInterval is pg_replication.txt's cadence.
const DefaultReplicationInterval = 10 * time.Second

// replicationColumns is the server's merge contract. pid leads because it is the
// stitching key across the window's samples: application_name is the natural
// display label but PostgreSQL does not enforce it unique per replica, so it is
// captured beside pid rather than instead of it.
//
// pid is not stable across a reconnect - a WAL sender that drops and comes back
// gets a new backend PID, and the series then shows one replica leaving and
// another arriving mid-window. That is what happened, and application_name and
// client_addr are the identity that survives it.
//
// The three lag columns are renamed: pg_stat_replication has write_lag,
// flush_lag and replay_lag as interval, and an interval string parses in about
// one language. Seconds, and the name says so.
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

// slotColumns is the same contract for the slots block, slot_name first: it is
// the join key here, and a better one than pid, being unique and stable across a
// restart.
var slotColumns = []string{
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

// sendersSQL reads the connected WAL senders. The view is column-identical on 14
// through 18 - read off pg_attribute on all five running servers on 2026-08-09,
// not taken from the documentation - so there is no variant and no capability
// flag, unlike the checkpoint counters.
//
// Every column whose type is outside int2/int4/int8, bool, text, float8 and
// timestamptz is cast in the statement rather than mapped in the scan. That is
// usesysid (oid), backend_xmin (xid) and the four pg_lsn columns. The oid cast
// is load-bearing rather than cosmetic: pgx v5.10.0 offers no scan plan from oid
// into a nullable *int32, so a bare usesysid mid-scan would cost the whole
// block on exactly the clusters this artifact exists for.
//
// client_addr is unwrapped with host() rather than cast, for the reason
// serverFactsSQL already gives about inet_server_addr(): the cast renders
// 10.0.4.12/32 and the /32 is an artifact of the return type.
//
// EXTRACT(EPOCH FROM ...) returns numeric on 14 and on 18, which pgx scans into
// a *float64 without complaint. The ::float8 is insurance rather than repair: it
// pins the wire type at the server rather than leaving it to pgtype.Numeric and
// a driver upgrade.
//
// ORDER BY pid, never on a lag: ordering on a statistic hands the server a
// different row set per sample, and this block is written on every one of them.
//
// No LIMIT and no count(*) OVER (), which is deliberate and is the first
// collector in this package without either. The row source is bounded by
// max_wal_senders, a GUC that sizes shared memory at startup, so a cap would
// guard a number the server already guards - and truncated=false on every
// capture ever taken is not a header key worth having.
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

// slotsSQL reads the replication slots. The same cast rule as above applies to
// datoid (oid), xmin and catalog_xmin (xid) and the two pg_lsn columns.
//
// ORDER BY s.slot_name and no cap, for the reasons sendersSQL gives; the bound
// here is max_replication_slots.
const slotsSQL = `SELECT s.slot_name::text,
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
       s.two_phase
FROM pg_catalog.pg_replication_slots s
ORDER BY s.slot_name`

// Replication captures the primary's replication picture: which replicas are
// connected and how far behind each is, and which slots are retaining WAL.
//
// Both views are readable, but not equally: pg_replication_slots needs no grant
// at all, where pg_stat_replication returns its rows to any role and masks every
// column past application_name to NULL without one. So a least-privilege capture
// of a healthy cluster and of one whose replica is an hour behind are
// byte-identical in the lag columns. Nothing here can tell them apart -
// pg_metadata.txt's has_pg_monitor_role is the discriminator, and the report
// joins the two.
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

		// No SampleBudget, and the absence is deliberate: two statements is
		// exactly DefaultSampleBudget, and an interval collector's offsets are
		// strictly inside the window, so this one never reaches the closing tick
		// moduleDeadline sums budgets for. Declaring one would size a tick this
		// collector cannot land on.
	}
}

// Sample reads the two views and writes one block each.
//
// Each block turns its own read failure into an error= header and an empty body,
// so a denied or timed-out read never costs the read that succeeded beside it.
// An error from here means the write failed, not that either read did.
func (r Replication) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	// Two blocks, one buffer, one Write: a write failing between them would
	// leave the window's stub block behind a half-written sample.
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

func (r Replication) writeSlotsBlock(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	rows, err := readSlots(ctx, q)

	fields := []headerField{
		{"db", s.Database},
		{"dbid", s.DBID},
		{"sample", strconv.Itoa(s.Index)},
	}

	if err != nil {
		fields = append(fields, headerField{"error", s.errorText(err)})
	}

	if err := writeBlockHeader(w, "pg_replication_slots", r.Artifact().Scope, fields, s.At); err != nil {
		return err
	}

	return writeRows(w, slotColumns, slotCells(rows))
}

// senderRow is one connected WAL sender. pid is the join key and a non-pointer,
// as bloatRow.relid and healthRow.datid are: a NULL there should cost the
// statement rather than write a keyless row into a series keyed on it.
//
// That is safe because it is measured rather than assumed - pid survives the
// masking a role holding only LOGIN sees (§3.6), where state and every LSN and
// lag column do not. Every other field is a pointer for exactly that reason: the
// masked case has to render as sixteen empty cells, not as a scan error costing
// the whole block.
//
// And an empty lag cell is not zero lag. It is NULL when the replica has not
// reported since the last flush, and NULL for every row on a role without
// pg_monitor.
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
// for senderRow.pid's reason; every other field is a pointer because a physical
// slot carries a lot of NULLs that all mean something. plugin, datoid, database
// and confirmed_flush_lsn are NULL for a physical slot by definition; xmin and
// catalog_xmin are NULL for a slot holding no snapshot; restart_lsn and
// wal_status are both NULL for a slot that never reserved WAL.
//
// safe_wal_size is NULL for every slot in the cluster when max_slot_wal_keep_size
// is at its -1 default, and populated for every non-invalidated slot when it is
// not. It is a cluster GUC rather than a per-slot property, so this column is
// all-empty or all-populated across the block and never mixed.
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
}

func readSlots(ctx context.Context, q RowQuerier) ([]slotRow, error) {
	stmtCtx, cancel := context.WithTimeout(ctx, StatementTimeout)
	defer cancel()

	rows, err := q.Query(stmtCtx, slotsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []slotRow

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
