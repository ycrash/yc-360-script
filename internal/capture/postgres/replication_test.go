package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	colSenderPID = iota
	colSenderUsesysid
	colSenderUsename
	colSenderApplicationName
	colSenderClientAddr
	colSenderClientHostname
	colSenderClientPort
	colSenderBackendStart
	colSenderBackendXmin
	colSenderState
	colSenderSentLSN
	colSenderWriteLSN
	colSenderFlushLSN
	colSenderReplayLSN
	colSenderWriteLag
	colSenderFlushLag
	colSenderReplayLag
	colSenderSyncPriority
	colSenderSyncState
	colSenderReplyTime
)

const (
	colSlotName = iota
	colSlotPlugin
	colSlotType
	colSlotDatoid
	colSlotDatabase
	colSlotTemporary
	colSlotActive
	colSlotActivePID
	colSlotXmin
	colSlotCatalogXmin
	colSlotRestartLSN
	colSlotConfirmedFlushLSN
	colSlotWALStatus
	colSlotSafeWALSize
	colSlotTwoPhase
	colSlotConflicting
	colSlotFailover
	colSlotInactiveSince
	colSlotInvalidationReason
	colSlotSynced
	colSlotTwoPhaseAt
)

const pg18OptionalColumns = "conflicting,failover,inactive_since,invalidation_reason,synced,two_phase_at"

var testSlotInactiveSince = time.Date(2026, 8, 7, 11, 2, 41, 330*int(time.Millisecond), time.UTC)

var testWALSenderStart = time.Date(2026, 8, 7, 9, 14, 2, 114*int(time.Millisecond), time.UTC)

func ordersWALSender(sent, write, flush, replay string,
	writeLag, flushLag, replayLag float64, reply time.Time,
) []any {
	return []any{
		int32(4021),
		ptr("16385"), ptr("replicator"), ptr("replica-01"),
		ptr("10.0.4.12"), nil, ptr(int32(54432)),
		&testWALSenderStart, nil, ptr("streaming"),
		ptr(sent), ptr(write), ptr(flush), ptr(replay),
		ptr(writeLag), ptr(flushLag), ptr(replayLag),
		ptr(int32(0)), ptr("async"), &reply,
	}
}

func ordersSenders1() [][]any {
	return [][]any{ordersWALSender(
		"2A/B4001200", "2A/B4001200", "2A/B4001100", "2A/B4000F80",
		0.4, 0.9, 1.2, at(32, 4, 902))}
}

func ordersSenders2() [][]any {
	return [][]any{ordersWALSender(
		"2A/B41C4880", "2A/B41C4880", "2A/B41C4880", "2A/B41C4300",
		0.31, 0.62, 0.88, at(32, 14, 941))}
}

func ordersSenders3() [][]any {
	return [][]any{ordersWALSender(
		"2A/B4380140", "2A/B4380140", "2A/B4379F00", "2A/B4371880",
		1.05, 2.44, 4.02, at(32, 24, 918))}
}

func ordersSlots(restartLSN, confirmedFlushLSN string, safeWALSize int64,
	inactiveSince *time.Time, optionalColumns string,
) [][]any {
	present := func(v any) any {
		if optionalColumns == "" {
			return nil
		}

		return v
	}

	probe := any(nil)
	if optionalColumns != "" {
		probe = ptr(optionalColumns)
	}

	return [][]any{
		{
			"orders_cdc_slot", ptr("pgoutput"), ptr("logical"), ptr("16401"), ptr("orders_db"),
			ptr(false), ptr(false), nil, nil, ptr("5518821"),
			ptr(confirmedFlushLSN), ptr(confirmedFlushLSN), ptr("extended"), ptr(safeWALSize), ptr(true),
			present(ptr(false)), present(ptr(false)), present(inactiveSince), nil,
			present(ptr(false)), nil,
			probe,
		},
		{
			"replica_01_slot", nil, ptr("physical"), nil, nil,
			ptr(false), ptr(true), ptr(int32(4021)), nil, nil,
			ptr(restartLSN), nil, ptr("reserved"), ptr(int64(3221225472)), ptr(false),
			nil, present(ptr(false)), nil, nil, present(ptr(false)), nil,
			probe,
		},
	}
}

func ordersSlotsPG18(restartLSN, confirmedFlushLSN string, safeWALSize int64) [][]any {
	return ordersSlots(restartLSN, confirmedFlushLSN, safeWALSize,
		&testSlotInactiveSince, pg18OptionalColumns)
}

type fakeReplicationConn struct {
	*fakeWindowConn

	senders []fakeResult
	slots   []fakeResult

	sql       []string
	slotsArgs [][]any
}

func newFakeReplicationConn() *fakeReplicationConn {
	return &fakeReplicationConn{
		fakeWindowConn: newFakeWindowConn(),
		senders: queue(
			rowsResult(ordersSenders1()),
			rowsResult(ordersSenders2()),
			rowsResult(ordersSenders3()),
		),
		slots: queue(
			rowsResult(ordersSlotsPG18("2A/B3FF0000", "2A/A1002000", 1073741824)),
			rowsResult(ordersSlotsPG18("2A/B41C0000", "2A/A1002000", 1071644672)),
			rowsResult(ordersSlotsPG18("2A/B4370000", "2A/A1002000", 1069547520)),
		),
	}
}

func (c *fakeReplicationConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = append(c.sql, sql)

	switch sql {
	case sendersSQL:
		return answer(&c.senders)

	case slotsSQL:
		c.slotsArgs = append(c.slotsArgs, args)

		return answer(&c.slots)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func replicationGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 61),
		at(32, 5, 61),
		at(32, 15, 58),
		at(32, 15, 58),
		at(32, 25, 55),
		at(32, 35, 2),
	)
}

func runReplicationWindow(t *testing.T, clock *scriptedClock, collector Replication,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   30 * time.Second,
		Collectors: []Collector{collector},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func replicationSampleContext(index int) SampleContext {
	return SampleContext{
		At: at(32, 5, 61), Index: index, Total: 3,
		Database: "orders_db", DBID: "16401",
	}
}

func takeReplicationSample(t *testing.T, conn *fakeReplicationConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, Replication{}.Sample(context.Background(), conn, &buf, replicationSampleContext(1)))

	return buf.String()
}

func TestReplicationArtifact(t *testing.T) {
	artifact := Replication{}.Artifact()

	assert.Equal(t, "pg_replication", artifact.Name)
	assert.Equal(t, "pg_replication.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"WAL senders and slots are the server's, not the connected database's")

	assert.Equal(t, Every(DefaultReplicationInterval), artifact.Schedule)
	assert.Equal(t, 10*time.Second, DefaultReplicationInterval, "12 samples on the default window")

	assert.Zero(t, artifact.SampleBudget,
		"two statements is exactly DefaultSampleBudget, and an interval collector's offsets are "+
			"strictly inside the window - so it never reaches the closing tick moduleDeadline "+
			"sums budgets for, and has nothing to declare")

	assert.Equal(t, Every(2*time.Second), Replication{Interval: 2 * time.Second}.Artifact().Schedule,
		"and a configured interval is carried through")
}

func TestReplicationColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
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
	}, replicationColumns)

	assert.Equal(t, "pid", replicationColumns[0],
		"the stitching key leads: application_name is not unique per replica")

	assert.Equal(t, []string{
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
		"conflicting",
		"failover",
		"inactive_since",
		"invalidation_reason",
		"synced",
		"two_phase_at",
	}, slotColumns, "the union of all five versions, identical on every one of them")

	assert.Equal(t, "slot_name", slotColumns[0],
		"the join key leads here too, and it is a better one than pid: slot names are unique "+
			"and survive a restart")

	assert.Equal(t, stableSlotColumns, slotColumns[:len(stableSlotColumns)],
		"the optional names are appended to the stable set rather than restated, so the column "+
			"header cannot promise a column the body does not carry")
	assert.Equal(t, optionalSlotColumnNames(), slotColumns[len(stableSlotColumns):])
}

func TestReplicationOptionalColumnsAreOneDeclaration(t *testing.T) {
	conn := newFakeReplicationConn()

	takeReplicationSample(t, conn)

	require.Len(t, conn.slotsArgs, 1)
	require.Equal(t, []any{optionalSlotColumnNames()}, conn.slotsArgs[0],
		"$1 is the same slice's names: settingNames()' analogue")

	for _, column := range optionalSlotColumns {
		assert.Contains(t, conn.slotsArgs[0][0], column.name,
			"%s is asked about but never selected", column.name)
		assert.Contains(t, slotsSQL, column.expr,
			"%s's expression did not survive assembly", column.name)
		assert.Contains(t, slotColumns, column.name,
			"%s is selected but absent from the column header", column.name)
	}

	assert.Len(t, optionalSlotColumns, 6,
		"conflicting at 16; failover, inactive_since, invalidation_reason and synced at 17; "+
			"two_phase_at at 18 - measured on all five servers on 2026-08-09")

	assert.Contains(t, slotsSQL, "AND attname = ANY($1::text[])",
		"the presence set is read from pg_attribute in the same statement, not from a second "+
			"one and not from a capability flag on SampleContext")
}

func TestReplicationOptionalColumnsHeaderKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slots [][]any
		want  string
	}{
		{
			name:  "PostgreSQL 18 has all six, and the header says so",
			slots: ordersSlotsPG18("2A/B3FF0000", "2A/A1002000", 1073741824),
			want:  "optional_columns=" + pg18OptionalColumns,
		},
		{
			name:  "PostgreSQL 16 has one",
			slots: ordersSlots("2A/B3FF0000", "2A/A1002000", 1073741824, nil, "conflicting"),
			want:  "optional_columns=conflicting",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newFakeReplicationConn()
			conn.slots = repeat(rowsResult(tc.slots))

			assert.Contains(t,
				capacityBlocks(t, takeReplicationSample(t, conn))["pg_replication_slots"].header,
				tc.want)
		})
	}

	t.Run("PostgreSQL 14 and 15 have none, so the key is absent rather than empty", func(t *testing.T) {
		conn := newFakeReplicationConn()
		conn.slots = repeat(rowsResult(ordersSlots("2A/B3FF0000", "2A/A1002000", 1073741824, nil, "")))

		block := capacityBlocks(t, takeReplicationSample(t, conn))["pg_replication_slots"]

		assert.NotContains(t, block.header, "optional_columns",
			"string_agg over no matching rows is NULL, and a NULL header value means the key "+
				"is never written")

		rows := block.rows(t, slotColumns)
		require.Len(t, rows, 2)

		for i := colSlotConflicting; i < len(slotColumns); i++ {
			assert.Empty(t, rows[0][i],
				"%s: the extraction yields NULL on a server without the column rather than "+
					"raising, which is what makes one statement cover all five versions",
				slotColumns[i])
		}
	})

	t.Run("and it is absent with no rows at all, on every version", func(t *testing.T) {
		conn := newFakeReplicationConn()
		conn.slots = repeat(rowsResult(nil))

		assert.NotContains(t,
			capacityBlocks(t, takeReplicationSample(t, conn))["pg_replication_slots"].header,
			"optional_columns",
			"a per-row scalar cannot survive an empty view, and that is correct here: with no "+
				"rows there are no empty cells to disambiguate, so the key is absent exactly "+
				"when it would have nothing to say")
	})
}

func TestReplicationInactiveSinceRendersInTheArtifactsTimestampForm(t *testing.T) {
	rows := capacityBlocks(t, takeReplicationSample(t, newFakeReplicationConn()))["pg_replication_slots"].
		rows(t, slotColumns)
	require.Len(t, rows, 2)

	assert.Equal(t, "2026-08-07T11:02:41.330Z", rows[0][colSlotInactiveSince],
		"which is what the ::timestamptz cast buys: without it the jsonb extraction arrives as "+
			"2026-08-07T11:02:41.330000+00:00, where every other timestamp in every artifact "+
			"is this form")
	assert.Empty(t, rows[1][colSlotInactiveSince],
		"and the connected slot has never been inactive")

	assert.Contains(t, slotsSQL, `(to_jsonb(s) ->> 'inactive_since')::timestamptz`)
}

func TestReplicationOptionalColumnsSeparateAbsentFromNotApplicable(t *testing.T) {
	block := capacityBlocks(t, takeReplicationSample(t, newFakeReplicationConn()))["pg_replication_slots"]

	rows := block.rows(t, slotColumns)
	require.Len(t, rows, 2)

	assert.Equal(t, "false", rows[0][colSlotConflicting],
		"the logical slot is not in conflict")
	assert.Empty(t, rows[1][colSlotConflicting],
		"and conflicting does not apply to a physical slot, so it is NULL on 18 for the same "+
			"reason it is NULL on 14 - where the column does not exist")

	assert.Contains(t, block.header, "optional_columns=",
		"which is the only thing that tells those two empty cells apart, and it is a fact "+
			"about the server rather than about the slot")
}

func TestReplicationWritesBothBlocksOnEverySample(t *testing.T) {
	sample := takeReplicationSample(t, newFakeReplicationConn())

	blocks := capacityBlocks(t, sample)
	require.Len(t, blocks, 2)

	assert.Less(t, strings.Index(sample, "source=pg_stat_replication"),
		strings.Index(sample, "source=pg_replication_slots"),
		"senders first, and the sample= key is what groups the two")

	for _, source := range []string{"pg_stat_replication", "pg_replication_slots"} {
		assert.Contains(t, blocks[source].header, "sample=1", source)
		assert.Contains(t, blocks[source].header, "scope=cluster", source)
		assert.Contains(t, blocks[source].header, "db=orders_db dbid=16401", source)
	}

	assert.Len(t, blocks["pg_stat_replication"].rows(t, replicationColumns), 1)
	assert.Len(t, blocks["pg_replication_slots"].rows(t, slotColumns), 2)
}

func TestReplicationWritesTheWholeSampleInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Replication{}.Sample(context.Background(), newFakeReplicationConn(), writer,
		replicationSampleContext(1)))

	assert.Equal(t, 1, writer.writes,
		"two blocks, one buffer, one Write: a write failing between them would leave the "+
			"window's stub behind a half-written sample")
	assert.Equal(t, 2, strings.Count(writer.buf.String(), "# engine=postgres"))
}

func TestReplicationZeroRowsWritesTheColumnHeadersAlone(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(rowsResult(nil))
	conn.slots = repeat(rowsResult(nil))

	blocks := capacityBlocks(t, takeReplicationSample(t, conn))

	for source, columns := range map[string][]string{
		"pg_stat_replication":  replicationColumns,
		"pg_replication_slots": slotColumns,
	} {
		assert.Empty(t, blocks[source].rows(t, columns), source)
		assert.NotContains(t, blocks[source].header, "error=",
			"%s: a column header with no rows is captured-and-found-nothing, and the absence of "+
				"error= is what separates that from could-not-be-captured", source)
	}

	assert.Equal(t, []string{strings.Join(replicationColumns, ",")},
		blocks["pg_stat_replication"].body,
		"which is what most captures look like: no replication configured is a finding, "+
			"not an absence")
}

func TestReplicationMaskedRowScansRatherThanCostingTheBlock(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(rowsResult([][]any{{
		int32(4021),
		ptr("16385"), ptr("replicator"), ptr("replica-01"),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	}}))

	block := capacityBlocks(t, takeReplicationSample(t, conn))["pg_stat_replication"]

	assert.NotContains(t, block.header, "error=",
		"a role holding only LOGIN is not denied this view: it sees the rows and every column "+
			"past application_name masked to NULL")

	rows := block.rows(t, replicationColumns)
	require.Len(t, rows, 1)

	assert.Equal(t, []string{"4021", "16385", "replicator", "replica-01"},
		rows[0][:colSenderClientAddr],
		"the four columns masking leaves alone, which is what makes pid safe as a non-pointer "+
			"destination")

	for i := colSenderClientAddr; i < len(replicationColumns); i++ {
		assert.Empty(t, rows[0][i], "%s survived masking", replicationColumns[i])
	}

	assert.Empty(t, rows[0][colSenderReplayLag],
		"and an empty lag is not zero lag - it is nobody was allowed to look. Nothing in this "+
			"artifact says which; pg_metadata.txt's has_pg_monitor_role does")
}

func TestReplicationNullJoinKeyCostsTheBlockRatherThanWritingAKeylessRow(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(rowsResult([][]any{{
		nil,
		ptr("16385"), ptr("replicator"), ptr("replica-01"),
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	}}))

	block := capacityBlocks(t, takeReplicationSample(t, conn))["pg_stat_replication"]

	assert.Contains(t, block.header, "error=",
		"pid is the key the window's twelve samples are stitched into a series on, so a NULL "+
			"there costs the statement rather than writing a row nothing can join")
	assert.Empty(t, block.rows(t, replicationColumns))
}

func TestReplicationBlocksFailIndependently(t *testing.T) {
	denied := errors.New("ERROR: permission denied for view pg_replication_slots (SQLSTATE 42501)")
	timedOut := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")

	t.Run("the slots read alone", func(t *testing.T) {
		conn := newFakeReplicationConn()
		conn.slots = repeat(errResult(denied))

		blocks := capacityBlocks(t, takeReplicationSample(t, conn))

		assert.Contains(t, blocks["pg_replication_slots"].header,
			`error="ERROR: permission denied for view pg_replication_slots (SQLSTATE 42501)"`,
			"driver text is quoted, so it cannot break k=v tokenisation")
		assert.Empty(t, blocks["pg_replication_slots"].rows(t, slotColumns))

		assert.Len(t, blocks["pg_stat_replication"].rows(t, replicationColumns), 1,
			"the senders read beside it is unaffected")
	})

	t.Run("the senders read alone", func(t *testing.T) {
		conn := newFakeReplicationConn()
		conn.senders = repeat(errResult(timedOut))

		blocks := capacityBlocks(t, takeReplicationSample(t, conn))

		assert.Contains(t, blocks["pg_stat_replication"].header, "error=")
		assert.Empty(t, blocks["pg_stat_replication"].rows(t, replicationColumns))

		assert.Len(t, blocks["pg_replication_slots"].rows(t, slotColumns), 2,
			"and the slots are still captured, which is the half carrying the WAL-retention "+
				"finding")
	})

	t.Run("both reads", func(t *testing.T) {
		conn := newFakeReplicationConn()
		conn.senders = repeat(errResult(timedOut))
		conn.slots = repeat(errResult(denied))

		var buf bytes.Buffer
		require.NoError(t, Replication{}.Sample(context.Background(), conn, &buf,
			replicationSampleContext(1)),
			"two refused reads are not an error: Sample fails only when it cannot write")

		blocks := capacityBlocks(t, buf.String())
		require.Len(t, blocks, 2,
			"two header-only blocks carrying two reasons, rather than one stub saying the "+
				"sample could not be taken")

		for source, columns := range map[string][]string{
			"pg_stat_replication":  replicationColumns,
			"pg_replication_slots": slotColumns,
		} {
			assert.Contains(t, blocks[source].header, "error=", source)
			assert.Empty(t, blocks[source].rows(t, columns), source)
		}
	})
}

func TestReplicationFailedSampleIsStillACompleteSample(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(errResult(errors.New("ERROR: permission denied")))
	conn.slots = repeat(errResult(errors.New("ERROR: permission denied")))

	results := runReplicationWindow(t, replicationGoldenClock(t), Replication{}, connectTo(conn))

	assert.Equal(t, StatusComplete, results[0].Status,
		"a sample of degraded blocks is a sample: the window's stub is for a collector that "+
			"cannot localise a failure, and this one always can")
	assert.Equal(t, 3, results[0].SamplesWritten)
	assert.NotContains(t, artifactText(t, results[0]), "sample_error=", "so no stub was written")
}

func TestReplicationSampleErrorsOnlyOnAWriteFailure(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	err := Replication{}.Sample(context.Background(), newFakeReplicationConn(),
		failingWriter{err: sinkErr}, replicationSampleContext(1))

	require.ErrorIs(t, err, sinkErr, "which the window turns into IOErr rather than into a stub")
}

func TestReplicationLagRendersAsSecondsAndNilRendersEmpty(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(rowsResult([][]any{{
		int32(4021),
		ptr("16385"), ptr("replicator"), ptr("replica-01"),
		ptr("10.0.4.12"), nil, ptr(int32(54432)),
		&testWALSenderStart, nil, ptr("streaming"),
		ptr("2A/B4001200"), ptr("2A/B4001200"), ptr("2A/B4001100"), ptr("2A/B4000F80"),
		ptr(0.000123), ptr(0.0), nil,
		ptr(int32(0)), ptr("async"), nil,
	}}))

	rows := capacityBlocks(t, takeReplicationSample(t, conn))["pg_stat_replication"].
		rows(t, replicationColumns)
	require.Len(t, rows, 1)

	assert.Equal(t, "0.000123", rows[0][colSenderWriteLag],
		"seconds, not PostgreSQL's 00:00:00.000123 - an interval string parses in about one "+
			"language, and the column name carries the unit")
	assert.Equal(t, "0", rows[0][colSenderFlushLag],
		"a zero lag is a reading and survives as 0")
	assert.Empty(t, rows[0][colSenderReplayLag],
		"where a NULL is no report since the last flush, which is not the same fact")
	assert.Empty(t, rows[0][colSenderReplyTime],
		"the only column that dates the lag figures beside it, and empty when it never arrived")
}

func TestReplicationSlotNullsAreMeaningfulAndNeverZero(t *testing.T) {
	rows := capacityBlocks(t, takeReplicationSample(t, newFakeReplicationConn()))["pg_replication_slots"].
		rows(t, slotColumns)
	require.Len(t, rows, 2)

	logical, physical := rows[0], rows[1]

	assert.Equal(t, "orders_cdc_slot", logical[colSlotName], "ordered by slot_name, so o precedes r")
	assert.Equal(t, []string{"pgoutput", "logical", "16401", "orders_db"},
		logical[colSlotPlugin:colSlotTemporary])
	assert.Equal(t, "false", logical[colSlotActive],
		"an abandoned logical slot with no consumer: this is the WAL-exhaustion incident, and "+
			"pg_metadata.txt's replication_configured reports false about exactly this cluster")
	assert.Empty(t, logical[colSlotActivePID])

	assert.Equal(t, "replica_01_slot", physical[colSlotName])
	for _, column := range []int{colSlotPlugin, colSlotDatoid, colSlotDatabase, colSlotConfirmedFlushLSN} {
		assert.Empty(t, physical[column],
			"%s is NULL for a physical slot by definition, and empty means not applicable, "+
				"never zero", slotColumns[column])
	}

	assert.Equal(t, "4021", physical[colSlotActivePID],
		"which equals pid in the block above it, and that join is why both blocks are in one "+
			"artifact")
	assert.Equal(t, "reserved", physical[colSlotWALStatus])
	assert.Equal(t, "3221225472", physical[colSlotSafeWALSize],
		"the bytes remaining before this slot is lost")
}

func TestReplicationStatementsAreOrderedOnAKeyAndUncapped(t *testing.T) {
	conn := newFakeReplicationConn()

	takeReplicationSample(t, conn)

	require.Equal(t, []string{sendersSQL, slotsSQL}, conn.sql,
		"two statements, which is what leaving SampleBudget at DefaultSampleBudget assumes")

	assert.Contains(t, sendersSQL, "ORDER BY pid")
	assert.Contains(t, slotsSQL, "ORDER BY s.slot_name")

	for name, sql := range map[string]string{"sendersSQL": sendersSQL, "slotsSQL": slotsSQL} {
		assert.NotContains(t, sql, "LIMIT",
			"%s: both row sources are bounded by GUCs that size shared memory at startup, so a "+
				"cap would guard a number the server already guards. This is the first collector "+
				"in the package without one and it is a decision, not an oversight", name)
		assert.NotContains(t, sql, "count(*) OVER ()",
			"%s: and with no cap there is nothing for a total to be compared against", name)
	}
}

func TestReplicationCastsEveryColumnTheDriverHasNoPlanFor(t *testing.T) {
	for _, cast := range []string{
		"usesysid::text",
		"backend_xmin::text",
		"sent_lsn::text",
		"write_lsn::text",
		"flush_lsn::text",
		"replay_lsn::text",
	} {
		assert.Contains(t, sendersSQL, cast,
			"oid, xid and pg_lsn are cast in the statement, not mapped in the scan")
	}

	for _, cast := range []string{
		"s.datoid::text",
		"s.xmin::text",
		"s.catalog_xmin::text",
		"s.restart_lsn::text",
		"s.confirmed_flush_lsn::text",
	} {
		assert.Contains(t, slotsSQL, cast)
	}

	assert.NotContains(t, sendersSQL, "\n       usesysid,",
		"pgx v5.10.0 has no scan plan from oid into a nullable *int32, and usesysid sits "+
			"mid-scan - so a bare selection would cost the whole block on every cluster that "+
			"has a WAL sender at all")
	assert.Contains(t, sendersSQL, "host(client_addr)",
		"unwrapped rather than cast: ::text renders 10.0.4.12/32 and the /32 is an artifact of "+
			"the return type")
}

func TestReplicationGoldenFull(t *testing.T) {
	results := runReplicationWindow(t, replicationGoldenClock(t), Replication{},
		connectTo(newFakeReplicationConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 3, results[0].SamplesWritten, "three samples, six sample blocks")
	assert.Equal(t, bloatGolden(t, "pg_replication_full.txt"), artifactText(t, results[0]))
}

func TestReplicationGoldenPre16(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.slots = queue(
		rowsResult(ordersSlots("2A/B3FF0000", "2A/A1002000", 1073741824, nil, "")),
		rowsResult(ordersSlots("2A/B41C0000", "2A/A1002000", 1071644672, nil, "")),
		rowsResult(ordersSlots("2A/B4370000", "2A/A1002000", 1069547520, nil, "")),
	)

	results := runReplicationWindow(t, replicationGoldenClock(t), Replication{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"one statement covers 14 through 18: the extraction returns NULL rather than raising")
	assert.Equal(t, bloatGolden(t, "pg_replication_pre16.txt"), artifactText(t, results[0]))
}

func TestReplicationGoldenNone(t *testing.T) {
	conn := newFakeReplicationConn()
	conn.senders = repeat(rowsResult(nil))
	conn.slots = repeat(rowsResult(nil))

	results := runReplicationWindow(t, replicationGoldenClock(t), Replication{}, connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"a standalone server is a complete capture that found nothing")
	assert.Equal(t, bloatGolden(t, "pg_replication_none.txt"), artifactText(t, results[0]))
}

func TestReplicationGoldenConnectFailure(t *testing.T) {
	clock := newScriptedClock(t, at(32, 4, 980), at(32, 9, 994))

	results := runReplicationWindow(t, clock, Replication{},
		func(context.Context, Target) (windowConn, error) { return nil, ErrTooManyConnections })

	require.Equal(t, StatusConnectFailed, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_replication_connect_failure.txt"), artifactText(t, results[0]))
}
