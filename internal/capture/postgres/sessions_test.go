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
	colSessionPID = iota
	colSessionDatid
	colSessionDatname
	colSessionLeaderPID
	colSessionUsesysid
	colSessionUsename
	colSessionApplicationName
	colSessionBackendType
	colSessionState
	colSessionWaitEventType
	colSessionWaitEvent
	colSessionBackendStart
	colSessionXactStart
	colSessionQueryStart
	colSessionStateChange
	colSessionBackendXID
	colSessionBackendXmin
	colSessionQueryID
	colSessionClientAddr
	colSessionClientHostname
	colSessionClientPort
	colSessionQuery
)

const (
	colLockPID = iota
	colLockType
	colLockDatabase
	colLockRelation
	colLockPage
	colLockTuple
	colLockVirtualXID
	colLockTransactionID
	colLockClassID
	colLockObjID
	colLockObjSubID
	colLockVirtualTransaction
	colLockMode
	colLockGranted
	colLockFastpath
	colLockWaitStart
)

const insufficientPrivilege = "<insufficient privilege>"

var (
	testBlockerStart = at(31, 58, 114)
	testWaiter1Start = at(32, 3, 1)
	testWaiter2Start = at(32, 3, 4)
	testAgentStart   = at(32, 4, 990)
)

func ordersChain(total int64) [][]any {
	return [][]any{
		{
			int32(1093), ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
			ptr("psql"), ptr("client backend"), ptr("active"), ptr("Timeout"), ptr("PgSleep"),
			&testBlockerStart, ptr(at(32, 1, 882)), ptr(at(32, 1, 884)), ptr(at(32, 1, 884)),
			ptr("789"), ptr("789"), ptr(int64(-4821096637582910234)),
			ptr("10.0.4.12"), nil, ptr(int32(54432)),
			ptr("BEGIN; UPDATE yc_bloat_orders SET status='blocker' WHERE id=300; SELECT pg_sleep(20);"),
			total,
		},
		{
			int32(1105), ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
			ptr("psql"), ptr("client backend"), ptr("active"), ptr("Lock"), ptr("transactionid"),
			&testWaiter1Start, ptr(at(32, 3, 140)), ptr(at(32, 3, 142)), ptr(at(32, 3, 142)),
			ptr("790"), ptr("789"), ptr(int64(5548219003471002234)),
			ptr("10.0.4.12"), nil, ptr(int32(54438)),
			ptr("UPDATE yc_bloat_orders SET status='waiter1' WHERE id=300;"),
			total,
		},
		{
			int32(1106), ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
			ptr("psql"), ptr("client backend"), ptr("active"), ptr("Lock"), ptr("tuple"),
			&testWaiter2Start, ptr(at(32, 3, 201)), ptr(at(32, 3, 203)), ptr(at(32, 3, 203)),
			ptr("791"), ptr("789"), ptr(int64(5548219003471002234)),
			ptr("10.0.4.12"), nil, ptr(int32(54440)),
			ptr("UPDATE yc_bloat_orders SET status='waiter2' WHERE id=300;"),
			total,
		},
	}
}

func ordersAgent(usesysid, usename string, clock time.Time, total int64) []any {
	return []any{
		int32(1116), ptr("16401"), ptr("orders_db"), nil, ptr(usesysid), ptr(usename),
		ptr(ApplicationName), ptr("client backend"), ptr("active"), nil, nil,
		&testAgentStart, &clock, &clock, &clock,
		nil, ptr("789"), ptr(int64(-7710234988821003345)),
		ptr("10.0.4.30"), nil, ptr(int32(54502)),
		ptr("SELECT pid, datid::text, datname::text, leader_pid, usesysid::text, ..."),
		total,
	}
}

func ordersSessions(clock time.Time) [][]any {
	const total = 4

	return append(ordersChain(total), ordersAgent("16385", "ycrash_monitor", clock, total))
}

func relationLock(pid int32, vxid, relation, mode string, total int64) []any {
	return []any{
		ptr(pid), ptr("relation"), ptr("16401"), ptr(relation),
		nil, nil, nil, nil, nil, nil, nil,
		ptr(vxid), ptr(mode), ptr(true), ptr(true), nil,
		total,
	}
}

func transactionLock(pid int32, vxid, xid, mode string, granted bool,
	waitstart *time.Time, total int64,
) []any {
	return []any{
		ptr(pid), ptr("transactionid"), nil, nil,
		nil, nil, nil, ptr(xid), nil, nil, nil,
		ptr(vxid), ptr(mode), ptr(granted), ptr(false), waitstart,
		total,
	}
}

func tupleLock(pid int32, vxid, relation string, page, tuple int32, granted bool,
	waitstart *time.Time, total int64,
) []any {
	return []any{
		ptr(pid), ptr("tuple"), ptr("16401"), ptr(relation),
		ptr(page), ptr(tuple), nil, nil, nil, nil, nil,
		ptr(vxid), ptr("ExclusiveLock"), ptr(granted), ptr(false), waitstart,
		total,
	}
}

func virtualXIDLock(pid int32, vxid string, total int64) []any {
	return []any{
		ptr(pid), ptr("virtualxid"), nil, nil,
		nil, nil, ptr(vxid), nil, nil, nil, nil,
		ptr(vxid), ptr("ExclusiveLock"), ptr(true), ptr(true), nil,
		total,
	}
}

func preparedTransactionLock(xid string, total int64) []any {
	return []any{
		nil, ptr("transactionid"), nil, nil,
		nil, nil, nil, ptr(xid), nil, nil, nil,
		ptr("-1/0"), ptr("ExclusiveLock"), ptr(true), ptr(false), nil,
		total,
	}
}

func ordersChainLocks() [][]any {
	const total = 17

	waiter1Wait := at(32, 3, 144)
	waiter2Wait := at(32, 3, 205)

	return [][]any{
		relationLock(1093, "4/1052", "16432", "RowExclusiveLock", total),
		relationLock(1093, "4/1052", "16439", "RowExclusiveLock", total),
		transactionLock(1093, "4/1052", "789", "ExclusiveLock", true, nil, total),
		virtualXIDLock(1093, "4/1052", total),

		relationLock(1105, "7/331", "16432", "RowExclusiveLock", total),
		relationLock(1105, "7/331", "16439", "RowExclusiveLock", total),
		transactionLock(1105, "7/331", "789", "ShareLock", false, &waiter1Wait, total),
		transactionLock(1105, "7/331", "790", "ExclusiveLock", true, nil, total),
		tupleLock(1105, "7/331", "16432", 1, 115, true, nil, total),
		virtualXIDLock(1105, "7/331", total),

		relationLock(1106, "9/12", "16432", "RowExclusiveLock", total),
		relationLock(1106, "9/12", "16439", "RowExclusiveLock", total),
		transactionLock(1106, "9/12", "791", "ExclusiveLock", true, nil, total),
		tupleLock(1106, "9/12", "16432", 1, 115, false, &waiter2Wait, total),
		virtualXIDLock(1106, "9/12", total),

		relationLock(1116, "3/88", "12073", "AccessShareLock", total),
		virtualXIDLock(1116, "3/88", total),
	}
}

func ordersIdleLocks() [][]any {
	const total = 2

	return [][]any{
		relationLock(1116, "3/88", "12073", "AccessShareLock", total),
		virtualXIDLock(1116, "3/88", total),
	}
}

func ordersQueryTextLocks() [][]any {
	const total = 6

	return [][]any{
		relationLock(1201, "12/401", "16432", "AccessShareLock", total),
		virtualXIDLock(1201, "12/401", total),
		relationLock(1202, "14/88", "16455", "RowExclusiveLock", total),
		virtualXIDLock(1202, "14/88", total),
		relationLock(1203, "15/22", "16432", "AccessShareLock", total),
		virtualXIDLock(1203, "15/22", total),
	}
}

func idleSession(total int64) []any {
	return []any{
		int32(1080), ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
		ptr("orders-service"), ptr("client backend"), ptr("idle"), ptr("Client"), ptr("ClientRead"),
		ptr(at(31, 40, 12)), nil, ptr(at(32, 4, 118)), ptr(at(32, 4, 118)),
		nil, nil, ptr(int64(2201884472100034410)),
		ptr("10.0.4.12"), nil, ptr(int32(54120)),
		ptr("SELECT id, status FROM yc_bloat_orders WHERE id = $1;"),
		total,
	}
}

func backgroundWorker(total int64) []any {
	return []any{
		int32(1042), nil, nil, nil, nil, nil,
		ptr(""), ptr("autovacuum launcher"), nil, ptr("Activity"), ptr("AutovacuumMain"),
		ptr(at(31, 12, 4)), nil, nil, nil,
		nil, nil, nil,
		nil, nil, nil,
		ptr(""),
		total,
	}
}

func ordersIdleSessions(clock time.Time) [][]any {
	const total = 3

	return [][]any{
		backgroundWorker(total),
		idleSession(total),
		ordersAgent("16385", "ycrash_monitor", clock, total),
	}
}

func maskedSession(pid int32, backendXID string, total int64) []any {
	return []any{
		pid, ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
		ptr("psql"), nil, nil, nil, nil,
		nil, nil, nil, nil,
		ptr(backendXID), ptr("789"), nil,
		nil, nil, nil,
		ptr(insufficientPrivilege),
		total,
	}
}

func ordersMasked(clock time.Time) [][]any {
	const total = 4

	return [][]any{
		maskedSession(1093, "789", total),
		maskedSession(1105, "790", total),
		maskedSession(1106, "791", total),
		ordersAgent("16386", "yc_restricted", clock, total),
	}
}

func sessionWithQuery(pid int32, query string, total int64) []any {
	return []any{
		pid, ptr("16401"), ptr("orders_db"), nil, ptr("10"), ptr("postgres"),
		ptr("orders-service"), ptr("client backend"), ptr("active"), nil, nil,
		&testWaiter1Start, nil, ptr(at(32, 4, 900)), ptr(at(32, 4, 900)),
		nil, ptr("789"), nil,
		ptr("10.0.4.12"), nil, ptr(int32(54000) + pid),
		ptr(query),
		total,
	}
}

func ordersQueryText() [][]any {
	const total = 3

	return [][]any{
		sessionWithQuery(1201,
			"SELECT o.id,\n       o.status\n  FROM orders o\n WHERE o.id = 300;", total),
		sessionWithQuery(1202,
			"INSERT INTO notes (body) VALUES ('\n# engine=postgres source=spoofed\nnot a header\n');", total),
		sessionWithQuery(1203,
			`SELECT "order,id", count(*) FROM "orders,archive" WHERE note = 'a "quoted" value, with a comma';`, total),
	}
}

type fakeSessionsConn struct {
	*fakeWindowConn

	sessions []fakeResult
	locks    []fakeResult

	sql          []string
	sessionsArgs [][]any
	locksArgs    [][]any
	deadlines    []time.Time
}

func newFakeSessionsConn() *fakeSessionsConn {
	return &fakeSessionsConn{
		fakeWindowConn: newFakeWindowConn(),
		sessions: queue(
			rowsResult(ordersSessions(at(32, 5, 61))),
			rowsResult(ordersSessions(at(32, 7, 55))),
			rowsResult(ordersSessions(at(32, 9, 52))),
		),
		locks: repeat(rowsResult(ordersChainLocks())),
	}
}

func (c *fakeSessionsConn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.sql = append(c.sql, sql)

	deadline, _ := ctx.Deadline()
	c.deadlines = append(c.deadlines, deadline)

	switch sql {
	case setSessionsTimeoutSQL, resetSessionsTimeoutSQL:
		return &fakeRows{}, nil

	case sessionsSQL:
		c.sessionsArgs = append(c.sessionsArgs, args)

		return answer(&c.sessions)

	case locksSQL:
		c.locksArgs = append(c.locksArgs, args)

		return answer(&c.locks)
	}

	return nil, fmt.Errorf("unexpected query: %s", sql)
}

func newFakeQueryTextConn() *fakeSessionsConn {
	conn := newFakeSessionsConn()
	conn.sessions = repeat(rowsResult(ordersQueryText()))
	conn.locks = repeat(rowsResult(ordersQueryTextLocks()))

	return conn
}

func sessionsGoldenClock(t *testing.T) *scriptedClock {
	return newScriptedClock(t,
		at(32, 4, 980),
		at(32, 5, 0),
		at(32, 5, 0),
		at(32, 5, 61),
		at(32, 5, 61),
		at(32, 7, 55),
		at(32, 7, 55),
		at(32, 9, 52),
		at(32, 11, 70),
	)
}

func runSessionsWindow(t *testing.T, clock *scriptedClock,
	connect func(ctx context.Context, target Target) (windowConn, error),
) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Target:     testTarget(),
		Duration:   6 * time.Second,
		Collectors: []Collector{Sessions{}},
		now:        clock.now,
		after:      clock.after,
		connect:    connect,
	}

	return window.Run(context.Background())
}

func sessionsSampleContext() SampleContext {
	return SampleContext{
		At: at(32, 5, 61), Index: 1, Total: 3,
		Database: "orders_db", DBID: "16401",
	}
}

func takeSessionsSample(t *testing.T, conn *fakeSessionsConn) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, Sessions{}.Sample(context.Background(), conn, &buf, sessionsSampleContext()))

	return buf.String()
}

func sessionsBlock(t *testing.T, conn *fakeSessionsConn) capacityBlock {
	t.Helper()

	return capacityBlocks(t, takeSessionsSample(t, conn))["pg_stat_activity"]
}

func locksBlock(t *testing.T, conn *fakeSessionsConn) capacityBlock {
	t.Helper()

	return capacityBlocks(t, takeSessionsSample(t, conn))["pg_locks"]
}

func TestSessionsArtifact(t *testing.T) {
	artifact := Sessions{}.Artifact()

	assert.Equal(t, "pg_sessions", artifact.Name)
	assert.Equal(t, "pg_sessions.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope,
		"pg_stat_activity is cluster-wide, so db= and dbid= mean connected through rather "+
			"than about - pg_health.txt's reading, and the one the capture direction confirms")

	assert.Equal(t, Every(DefaultSessionsInterval), artifact.Schedule)
	assert.Equal(t, 2*time.Second, DefaultSessionsInterval,
		"60 samples on the default window, and the count is the point: a blocking chain is a "+
			"thing that develops")

	assert.Equal(t, 3*time.Second, artifact.SampleBudget,
		"two statements at this collector's own timeout. Documentation rather than arithmetic: "+
			"SampleBudget is consulted only for the collectors due on the closing tick, which "+
			"an Every collector never reaches - the SET in Sample is what does the work")

	assert.Equal(t, Every(5*time.Second), Sessions{Interval: 5 * time.Second}.Artifact().Schedule,
		"and a configured interval is carried through")
}

func TestSessionsColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"pid",
		"datid",
		"datname",
		"leader_pid",
		"usesysid",
		"usename",
		"application_name",
		"backend_type",
		"state",
		"wait_event_type",
		"wait_event",
		"backend_start",
		"xact_start",
		"query_start",
		"state_change",
		"backend_xid",
		"backend_xmin",
		"query_id",
		"client_addr",
		"client_hostname",
		"client_port",
		"query",
	}, sessionColumns, "all twenty-two, identical on 14 through 18, where only nine were "+
		"asked for")

	assert.Equal(t, "pid", sessionColumns[0],
		"the stitching key leads - and it is what makes it impossible for a data line to begin "+
			"with '#', whatever the query text carries")
	assert.Equal(t, "query", sessionColumns[len(sessionColumns)-1],
		"and the one unbounded column closes, so every structured column of a row is legible "+
			"in a pager before the text runs off the right edge")
}

func TestSessionsLockColumnOrder(t *testing.T) {
	assert.Equal(t, []string{
		"pid",
		"locktype",
		"database",
		"relation",
		"page",
		"tuple",
		"virtualxid",
		"transactionid",
		"classid",
		"objid",
		"objsubid",
		"virtualtransaction",
		"mode",
		"granted",
		"fastpath",
		"waitstart",
	}, lockColumns, "all sixteen, identical on 14 through 18, where only ten were asked for")

	assert.Equal(t, "pid", lockColumns[0],
		"the join key leads here too, which keeps a backend's locks contiguous for a human")
	assert.Equal(t, "waitstart", lockColumns[len(lockColumns)-1],
		"and the one column nobody asked for closes: it is what lets a single "+
			"sample carry a wait's duration, where diffing consecutive samples is bounded "+
			"below by the 2s cadence")
}

func TestSessionsWritesBothBlocksOnEverySample(t *testing.T) {
	sample := takeSessionsSample(t, newFakeSessionsConn())

	blocks := capacityBlocks(t, sample)
	require.Len(t, blocks, 2)

	assert.Less(t, strings.Index(sample, "source=pg_stat_activity"),
		strings.Index(sample, "source=pg_locks"),
		"sessions first, and the sample= key is what groups the two")

	for _, source := range []string{"pg_stat_activity", "pg_locks"} {
		assert.Contains(t, blocks[source].header, "sample=1", source)
		assert.Contains(t, blocks[source].header, "scope=cluster", source)
		assert.Contains(t, blocks[source].header, "db=orders_db dbid=16401", source)
		assert.Contains(t, blocks[source].header, "ts=2026-08-07T14:32:05.061Z",
			"%s: both blocks of one sample carry the same ts=, which is one clock read per "+
				"sample by construction rather than the sampler catching up - so the server "+
				"treats a cross-block join within a sample as simultaneous", source)
	}

	assert.Len(t, blocks["pg_stat_activity"].rows(t, sessionColumns), 4)
	assert.Len(t, blocks["pg_locks"].rows(t, lockColumns), 17)
}

func TestSessionsLocksStatementIsTotallyOrderedAndCapped(t *testing.T) {
	conn := newFakeSessionsConn()

	takeSessionsSample(t, conn)

	assert.Contains(t, locksSQL,
		"ORDER BY pid, locktype, database, relation, page, tuple,\n"+
			"         virtualxid, transactionid::text, classid, objid, objsubid, mode,\n"+
			"         virtualtransaction",
		"the full lock identity plus the holder, because anything less was not total: two "+
			"advisory locks held by one session tie on (pid, locktype, relation, page, tuple, "+
			"mode) and come back in arbitrary order - measured on all five servers - and two "+
			"prepared transactions holding the same-tagged lock in the same mode tie on every "+
			"identity column, separated only by virtualtransaction, which is never NULL. A "+
			"nondeterministic cap boundary is what the ordering rule exists to prevent")

	assert.NotContains(t, locksSQL, "ORDER BY granted",
		"ordering so that ungranted rows survive the cap is the obvious idea and it is "+
			"rejected: granted is precisely a value that changes between samples, so it would "+
			"hand the server a different row set per sample and make a row's appearance an "+
			"artefact of the cap rather than of the lock")

	assert.Contains(t, locksSQL, "transactionid::text, classid",
		"transactionid sorts as its text cast, because xid has no ordering operator")

	assert.Contains(t, locksSQL, "LIMIT $1")
	assert.Contains(t, locksSQL, "count(*) OVER () AS locks_total")

	require.Len(t, conn.locksArgs, 1)
	assert.Equal(t, []any{DefaultMaxLocks}, conn.locksArgs[0])
	assert.Equal(t, 5000, DefaultMaxLocks)
}

func TestSessionsLockCastsEveryColumnTheDriverHasNoPlanFor(t *testing.T) {
	for _, cast := range []string{
		"database::text",
		"relation::text",
		"transactionid::text",
		"classid::text",
		"objid::text",
	} {
		assert.Contains(t, locksSQL, cast, "oid and xid are cast, never mapped in the scan")
	}

	for _, bare := range []string{"\n       page,\n", "\n       tuple,\n", "\n       objsubid,\n"} {
		assert.Contains(t, locksSQL, bare,
			"page is int4 and tuple and objsubid are int2, and pgx plans int2 into *int32 "+
				"without a cast - measured - so this block adds no renderer")
	}

	assert.NotContains(t, locksSQL, "regclass",
		"OIDs are captured raw. Measured on 18: relation::regclass::text on a lock belonging "+
			"to another database returns the OID's own digits rather than raising or NULL - "+
			"and had that OID existed in the connected database as some other table, it would "+
			"have resolved to that table's name, confidently and wrongly")
}

func TestSessionsChainRoundTripsThroughTheRenderer(t *testing.T) {
	rows := locksBlock(t, newFakeSessionsConn()).rows(t, lockColumns)
	require.Len(t, rows, 17)

	byKey := func(locktype, transactionID string, granted bool) []string {
		t.Helper()

		var found [][]string
		for _, row := range rows {
			if row[colLockType] == locktype &&
				row[colLockTransactionID] == transactionID &&
				row[colLockGranted] == fmt.Sprint(granted) {
				found = append(found, row)
			}
		}
		require.Len(t, found, 1, "%s %s granted=%v", locktype, transactionID, granted)

		return found[0]
	}

	waiting := byKey("transactionid", "789", false)
	holding := byKey("transactionid", "789", true)

	assert.Equal(t, "1105", waiting[colLockPID])
	assert.Equal(t, "1093", holding[colLockPID],
		"the first hop of the chain: an ungranted transactionid row and the granted row for "+
			"the same transactionid name the blocker. That is the join pg_blocking_pids() was "+
			"rejected in favour of, and this is the artifact proving it is performable from "+
			"what the agent wrote")

	assert.Equal(t, "2026-08-07T14:32:03.144Z", waiting[colLockWaitStart],
		"the wait's clock, so one sample carries its duration")
	assert.Empty(t, holding[colLockWaitStart],
		"and it is empty on every granted row - measured non-NULL on exactly the ungranted "+
			"ones, though the documentation lets it lag a wait's start briefly, so "+
			"granted=false stays the marker and waitstart the clock")

	var tuples [][]string
	for _, row := range rows {
		if row[colLockType] == "tuple" {
			tuples = append(tuples, row)
		}
	}
	require.Len(t, tuples, 2)

	assert.Equal(t, []string{"1105", "tuple", "16401", "16432", "1", "115"},
		tuples[0][:colLockVirtualXID])
	assert.Equal(t, "true", tuples[0][colLockGranted])
	assert.Equal(t, "1106", tuples[1][colLockPID])
	assert.Equal(t, "false", tuples[1][colLockGranted])
	assert.NotEmpty(t, tuples[1][colLockWaitStart],
		"the second hop, and it joins on (relation, page, tuple) rather than on a transaction "+
			"id - a chain of depth two already needs two different keys, which is why page and "+
			"tuple are captured rather than dropped as implementation detail")
}

func TestSessionsPreparedTransactionLockRendersWithNoPID(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.locks = repeat(rowsResult([][]any{
		relationLock(1093, "4/1052", "16432", "RowExclusiveLock", 2),
		preparedTransactionLock("805", 2),
	}))

	block := locksBlock(t, conn)

	assert.NotContains(t, block.header, "error=",
		"pg_locks.pid is NULL for a lock held by a prepared transaction - measured on 18 with "+
			"max_prepared_transactions=2 - so a non-pointer destination would cost the whole "+
			"block, sixty times, on exactly the clusters running two-phase commit")

	rows := block.rows(t, lockColumns)
	require.Len(t, rows, 2)

	anonymous := rows[1]
	assert.Empty(t, anonymous[colLockPID], "an anonymous holder, not a broken row")
	assert.Equal(t, "805", anonymous[colLockTransactionID],
		"and it joins by transactionid like any other, which is what keeps it in the chain")
	assert.Equal(t, "true", anonymous[colLockGranted])

	assert.True(t, strings.HasPrefix(block.body[2], ","),
		"an empty first cell begins the line with a comma - never with '#', which is what "+
			"keeps the no-data-line-starts-with-a-hash rule true for this block too")
}

func TestSessionsStatementIsUnfilteredOrderedAndCapped(t *testing.T) {
	conn := newFakeSessionsConn()

	takeSessionsSample(t, conn)

	assert.NotContains(t, sessionsSQL, "WHERE",
		"a WHERE pid <> pg_backend_pid() is the obvious ask; this statement has no WHERE "+
			"clause at all, and the generality is the point. Twelve of these mask to "+
			"NULL without pg_read_all_stats, so a filter on backend_type - the obvious one to "+
			"write - returns one row of ten on a least-privilege capture, silently. No WHERE "+
			"clause is the rule with no exception to remember")
	assert.NotContains(t, sessionsSQL, "pg_backend_pid")

	assert.Contains(t, sessionsSQL, "ORDER BY pid",
		"never on a value that moves between samples, and this block is written 60 times per "+
			"file")
	assert.Contains(t, sessionsSQL, "LIMIT $1")
	assert.Contains(t, sessionsSQL, "count(*) OVER () AS sessions_total",
		"evaluated before LIMIT, so it is the uncapped total")

	require.Len(t, conn.sessionsArgs, 1)
	assert.Equal(t, []any{DefaultMaxSessions, DefaultMaxQueryText + 1}, conn.sessionsArgs[0],
		"the second argument is left()'s bound - one rune past the agent's cap, so the render "+
			"pass can still detect and count a cut while the overage never crosses the wire")
	assert.Equal(t, []any{7, DefaultMaxQueryText + 1}, sessionsQueryArgs(t, Sessions{MaxSessions: 7}))
}

func sessionsQueryArgs(t *testing.T, collector Sessions) []any {
	t.Helper()

	conn := newFakeSessionsConn()

	var buf bytes.Buffer
	require.NoError(t, collector.Sample(context.Background(), conn, &buf, sessionsSampleContext()))
	require.Len(t, conn.sessionsArgs, 1)

	return conn.sessionsArgs[0]
}

func TestSessionsCastsEveryColumnTheDriverHasNoPlanFor(t *testing.T) {
	for _, cast := range []string{
		"datid::text",
		"usesysid::text",
		"backend_xid::text",
		"backend_xmin::text",
	} {
		assert.Contains(t, sessionsSQL, cast,
			"oid and xid are cast in the statement, not mapped in the scan: measured against "+
				"pgx v5.10.0, oid has no scan plan into **int32 and xid none into **int64")
	}

	assert.Contains(t, sessionsSQL, "host(client_addr)",
		"unwrapped rather than cast: inet has no scan plan into **string at all, and ::text "+
			"would render 10.0.4.12/32, where the /32 is an artifact of the return type")
	assert.NotContains(t, sessionsSQL, "client_addr::text")

	for _, cast := range []string{"datname::text", "usename::text"} {
		assert.Contains(t, sessionsSQL, cast,
			"name scans without a cast; cast anyway to match health.go's datname::text")
	}

	assert.NotContains(t, sessionsSQL, "\n       backend_xid,",
		"a bare xid would cost the whole block rather than a cell - and it would hide: pgx "+
			"scans a NULL into any destination, so it passes every fake and every idle cluster "+
			"and fails the first time a customer captures during a write transaction. The "+
			"matrix driving a non-NULL value through every cast column is the only test that "+
			"can catch a miss")
}

func TestSessionsStatementTimeoutIsServerSideAndAlwaysRestored(t *testing.T) {
	assert.Equal(t, 1500*time.Millisecond, SessionsStatementTimeout,
		"well under StatementTimeout, because this collector samples 60 times against the "+
			"same worst case the others meet twelve times or twice")
	assert.Equal(t, "SET statement_timeout TO '1500ms'", setSessionsTimeoutSQL,
		"formatted from the constant, so the literal the server sees and the Go value behind "+
			"SampleBudget cannot drift apart")

	t.Run("the SET precedes both reads and the RESET follows them", func(t *testing.T) {
		conn := newFakeSessionsConn()

		takeSessionsSample(t, conn)

		assert.Equal(t,
			[]string{setSessionsTimeoutSQL, sessionsSQL, locksSQL, resetSessionsTimeoutSQL},
			conn.sql)
	})

	t.Run("and the RESET runs after a failed read too", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(errResult(errors.New(
			"ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")))

		takeSessionsSample(t, conn)

		assert.Equal(t, resetSessionsTimeoutSQL, conn.sql[len(conn.sql)-1],
			"the session is shared with every other collector, so a sample hands it back with "+
				"the startup-packet value it found whatever happened to the read")
	})

	t.Run("and no statement's context deadline is ever the shorter constant", func(t *testing.T) {
		conn := newFakeSessionsConn()

		start := time.Now()
		takeSessionsSample(t, conn)

		require.Len(t, conn.deadlines, 4)

		for i, deadline := range conn.deadlines {
			assert.WithinDuration(t, start.Add(StatementTimeout), deadline, 5*time.Second,
				"statement %d: the reads' contexts stay at the package timeout", i)
			assert.Greater(t, deadline.Sub(start), SessionsStatementTimeout,
				"statement %d: the timeout is a server-side SET and never a context deadline. "+
					"pgx's default context watcher closes the connection when a context "+
					"expires, and the window has no reconnect - so the client-side form a "+
					"tidy-up would simplify this back to converts one slow read into the rest "+
					"of the run being dead", i)
		}
	})
}

func TestSessionsCapturesTheAgentsOwnBackend(t *testing.T) {
	rows := sessionsBlock(t, newFakeSessionsConn()).rows(t, sessionColumns)
	require.Len(t, rows, 4)

	assert.Equal(t, ApplicationName, rows[3][colSessionApplicationName],
		"the agent's own session is captured like any other and dropped by the server if it "+
			"wants to: excluding it here would make this artifact's connection count disagree "+
			"with pg_capacity.txt's by one, and the server cannot invent a row the agent "+
			"withheld")

	assert.Empty(t, rows[3][colSessionBackendXID],
		"its transaction has written nothing")
	assert.NotEmpty(t, rows[3][colSessionBackendXmin],
		"but it pins a snapshot, because a backend photographing itself is active inside its "+
			"own statement")
}

func TestSessionsMaskedRowScansRatherThanCostingTheBlock(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = repeat(rowsResult([][]any{maskedSession(1093, "789", 1)}))

	block := sessionsBlock(t, conn)

	assert.NotContains(t, block.header, "error=",
		"a role holding only LOGIN is not denied this view: it sees every row, twelve columns "+
			"masked, and nothing anywhere says so. That silence is the artifact's highest "+
			"report-side risk")
	assert.Contains(t, block.header, "sessions_written=1 sessions_total=1 truncated=false",
		"and the counts are right, which is what makes the file look complete")

	rows := block.rows(t, sessionColumns)
	require.Len(t, rows, 1)

	for _, column := range []int{
		colSessionPID, colSessionDatid, colSessionDatname, colSessionUsesysid,
		colSessionUsename, colSessionApplicationName, colSessionBackendXID, colSessionBackendXmin,
	} {
		assert.NotEmpty(t, rows[0][column],
			"%s is the measured identity floor: a least-privilege capture still says which "+
				"database, which role and which application - and, through backend_xid, which "+
				"session holds the transaction a chain is queued behind", sessionColumns[column])
	}

	var empty int
	for _, column := range []int{
		colSessionLeaderPID, colSessionBackendType, colSessionState, colSessionWaitEventType,
		colSessionWaitEvent, colSessionBackendStart, colSessionXactStart, colSessionQueryStart,
		colSessionStateChange, colSessionQueryID, colSessionClientAddr, colSessionClientHostname,
		colSessionClientPort,
	} {
		assert.Empty(t, rows[0][column], "%s survived masking", sessionColumns[column])
		empty++
	}

	assert.Equal(t, 13, empty,
		"thirteen empty cells and no error: the case a non-pointer scan destination would turn "+
			"into a lost block")

	assert.Equal(t, insufficientPrivilege, rows[0][colSessionQuery],
		"the first column in the feature where the server hands back a sentence instead of a "+
			"NULL, and it breaks the rule the rest of the package rests on. Captured verbatim "+
			"and never detected: rewriting it to empty would destroy the distinction between "+
			"denied and this backend has no current query, and string-matching a server-side "+
			"message is the inference the design principle forbids. pg_metadata.txt's "+
			"has_pg_read_all_stats is the discriminator")
}

func TestSessionsLocksNeedNoGrantWhereTheActivityViewMasks(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = repeat(rowsResult([][]any{maskedSession(1093, "789", 1)}))

	blocks := capacityBlocks(t, takeSessionsSample(t, conn))

	assert.Equal(t, insufficientPrivilege,
		blocks["pg_stat_activity"].rows(t, sessionColumns)[0][colSessionQuery],
		"the activity view masks for a role without pg_read_all_stats")

	locks := blocks["pg_locks"].rows(t, lockColumns)

	assert.Equal(t, locksBlock(t, newFakeSessionsConn()).rows(t, lockColumns), locks,
		"and the locks block beside it is byte-identical to the same cluster's read by a "+
			"pg_monitor member, because pg_locks needs no grant at all - measured on all five "+
			"servers, a role holding nothing but LOGIN sees the same rows and the same columns")

	require.Len(t, locks, 17)
	assert.Equal(t, "1105", locks[6][colLockPID])
	assert.Equal(t, "false", locks[6][colLockGranted],
		"so the two blocks degrade in opposite directions: at the privilege floor the capture "+
			"still shapes the chain - who holds what, and who is queued behind whom - while "+
			"being unable to quote a single statement")
}

func TestSessionsNullPIDCostsTheBlockRatherThanWritingAKeylessRow(t *testing.T) {
	conn := newFakeSessionsConn()

	nullPID := maskedSession(1093, "789", 1)
	nullPID[colSessionPID] = nil
	conn.sessions = repeat(rowsResult([][]any{nullPID}))

	block := sessionsBlock(t, conn)

	assert.Contains(t, block.header, "error=",
		"pid is the key 60 samples are stitched into a series on, so a NULL there costs the "+
			"statement rather than writing a row nothing can join. Safe as a non-pointer "+
			"because pg_stat_activity never NULLs it and because it survives masking, which is "+
			"what makes the choice right here where it would be wrong for state")
	assert.NotContains(t, block.header, "sessions_total",
		"and a failed read drops the count keys rather than writing zeroes")
	assert.Empty(t, block.rows(t, sessionColumns))
}

func TestSessionsCapsDeclareThemselvesInTheHeader(t *testing.T) {
	t.Run("under the cap", func(t *testing.T) {
		assert.Contains(t, sessionsBlock(t, newFakeSessionsConn()).header,
			"sessions_written=4 sessions_total=4 truncated=false")
	})

	t.Run("over it", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(rowsResult([][]any{
			maskedSession(1093, "789", 430),
			maskedSession(1105, "790", 430),
		}))

		block := sessionsBlock(t, conn)

		assert.Contains(t, block.header, "sessions_written=2 sessions_total=430 truncated=true",
			"ORDER BY pid ... LIMIT keeps the lowest PIDs - usually, but not always, the "+
				"oldest backends, since PIDs wrap - so a cap that binds during a connection "+
				"storm tends to shed the incident's arrivals first: truncated=true means "+
				"the tail of the story is missing, not that this is a uniform sample")
		assert.Len(t, block.rows(t, sessionColumns), 2)
	})

	t.Run("and the locks block carries its own three keys", func(t *testing.T) {
		assert.Contains(t, locksBlock(t, newFakeSessionsConn()).header,
			"locks_written=17 locks_total=17 truncated=false")

		wait := at(32, 3, 144)

		conn := newFakeSessionsConn()
		conn.locks = repeat(rowsResult([][]any{
			transactionLock(1105, "7/331", "789", "ShareLock", false, &wait, 8102),
		}))

		assert.Contains(t, locksBlock(t, conn).header,
			"locks_written=1 locks_total=8102 truncated=true",
			"an ungranted row is where it is because of ORDER BY pid, not because anything "+
				"protects it: nothing does, and that is the priced cost of ordering on a key "+
				"that does not move between samples")
	})
}

func TestSessionsQueryTextIsCappedByTheAgentAndCounted(t *testing.T) {
	t.Run("at the limit exactly, nothing happens and there is no key", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(rowsResult([][]any{
			sessionWithQuery(1093, strings.Repeat("a", DefaultMaxQueryText), 1),
		}))

		block := sessionsBlock(t, conn)

		assert.NotContains(t, block.header, "queries_truncated",
			"zero cells, no key: the package's rule for a conditional header")

		rows := block.rows(t, sessionColumns)
		require.Len(t, rows, 1)
		assert.Len(t, []rune(rows[0][colSessionQuery]), DefaultMaxQueryText)
	})

	t.Run("past it, the cell is marked and the row is counted", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(rowsResult([][]any{
			sessionWithQuery(1093, strings.Repeat("a", DefaultMaxQueryText+1), 2),
			sessionWithQuery(1105, "SELECT 1;", 2),
		}))

		block := sessionsBlock(t, conn)

		assert.Contains(t, block.header, "queries_truncated=1",
			"which is what keeps the agent's truncation distinguishable from the server's: the "+
				"server's ends a statement mid-token at track_activity_query_size with no "+
				"marker, and pg_metadata.txt records that limit")

		rows := block.rows(t, sessionColumns)
		require.Len(t, rows, 2)

		assert.Equal(t, strings.Repeat("a", DefaultMaxQueryText)+"...", rows[0][colSessionQuery])
		assert.Equal(t, "SELECT 1;", rows[1][colSessionQuery], "and the row beside it is untouched")
	})

	t.Run("the cap counts runes, so a multi-byte character is never split", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(rowsResult([][]any{
			sessionWithQuery(1093, "SELECT '"+strings.Repeat("é", DefaultMaxQueryText)+"';", 1),
		}))

		rows := sessionsBlock(t, conn).rows(t, sessionColumns)
		require.Len(t, rows, 1)

		assert.True(t, strings.HasSuffix(rows[0][colSessionQuery], "é..."))
		assert.Len(t, []rune(rows[0][colSessionQuery]), DefaultMaxQueryText+3)
	})

	assert.Contains(t, sessionsSQL, "left(query, $2) AS query",
		"the bound is applied in the statement, one rune past the cap, so the overage never "+
			"crosses the wire or sits in the row slice: without it a raised "+
			"track_activity_query_size ships MaxSessions megabyte rows per sample, and a "+
			"transfer that slow can outrun the 10s client backstop, which costs the run's "+
			"only connection")

	assert.Equal(t, 8192, DefaultMaxQueryText,
		"eight times the server's own default, so at default settings it never fires - the cap "+
			"exists for the GUC's 1,048,576-byte ceiling, at which a saturated sample would "+
			"buffer a gigabyte on a customer host")
}

func TestSessionsQueryTextIsOneLinePerRowWhateverItContains(t *testing.T) {
	conn := newFakeQueryTextConn()

	sample := takeSessionsSample(t, conn)

	lines := strings.Split(strings.TrimSuffix(sample, "\n"), "\n")

	var headers, data int
	for _, line := range lines {
		if strings.HasPrefix(line, "#") {
			headers++
		} else {
			data++
		}
	}

	assert.Equal(t, 2, headers, "two block headers, and no data line begins with '#'")
	assert.Equal(t, 11, data,
		"two column headers, three activity rows and six lock rows - a query carrying three "+
			"embedded newlines added no line of its own. singleLine is what buys that, and it "+
			"is what lets a line-oriented parser and a record-aware parser read this file "+
			"identically")

	assert.Contains(t, lines[3], "# engine=postgres source=spoofed",
		"a query can carry a line that looks exactly like a block header")
	assert.True(t, strings.HasPrefix(lines[3], "1202,"),
		"and it can never start one, because pid is the first cell. That is why pid leading is "+
			"a decision rather than an aesthetic")

	rows := capacityBlocks(t, sample)["pg_stat_activity"].rows(t, sessionColumns)
	require.Len(t, rows, 3)

	assert.Equal(t, "SELECT o.id,        o.status   FROM orders o  WHERE o.id = 300;",
		rows[0][colSessionQuery],
		"line breaks become spaces and nothing else does: the capture direction proposes "+
			"collapsing whitespace runs, and singleLine collapses breaks only, leaving the "+
			"statement otherwise as the application submitted it")

	assert.Equal(t,
		`SELECT "order,id", count(*) FROM "orders,archive" WHERE note = 'a "quoted" value, with a comma';`,
		rows[2][colSessionQuery],
		"commas and quotes round-trip through encoding/csv, which is why the bodies go through "+
			"it rather than a hand-rolled join")
}

func TestSessionsWritesTheWholeSampleInOneWrite(t *testing.T) {
	writer := &countingWriter{}

	require.NoError(t, Sessions{}.Sample(context.Background(), newFakeSessionsConn(), writer,
		sessionsSampleContext()))

	assert.Equal(t, 1, writer.writes,
		"two blocks, one buffer, one Write: a write failing between them would leave the "+
			"window's stub behind a half-written sample")
	assert.Equal(t, 2, strings.Count(writer.buf.String(), "# engine=postgres"))
}

func TestSessionsZeroRowsWritesTheColumnHeadersAlone(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = repeat(rowsResult(nil))
	conn.locks = repeat(rowsResult(nil))

	blocks := capacityBlocks(t, takeSessionsSample(t, conn))

	for source, columns := range map[string][]string{
		"pg_stat_activity": sessionColumns,
		"pg_locks":         lockColumns,
	} {
		assert.Empty(t, blocks[source].rows(t, columns), source)
		assert.NotContains(t, blocks[source].header, "error=",
			"%s: a column header with no rows is captured-and-found-nothing, and the absence "+
				"of error= is what separates that from could-not-be-captured", source)
	}

	assert.Equal(t, []string{strings.Join(sessionColumns, ",")},
		blocks["pg_stat_activity"].body)
}

func TestSessionsBlocksFailIndependently(t *testing.T) {
	timedOut := errors.New("ERROR: canceling statement due to statement timeout (SQLSTATE 57014)")
	denied := errors.New("ERROR: permission denied")

	t.Run("the activity read alone", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(errResult(timedOut))

		blocks := capacityBlocks(t, takeSessionsSample(t, conn))

		assert.Contains(t, blocks["pg_stat_activity"].header,
			`error="ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"`,
			"driver text is quoted, so it cannot break k=v tokenisation")
		assert.NotContains(t, blocks["pg_stat_activity"].header, "sessions_written",
			"and the count keys are dropped rather than written as zeroes: sessions_total=0 "+
				"would assert that the server has no backends, where the truth is that nobody "+
				"could count them")
		assert.Empty(t, blocks["pg_stat_activity"].rows(t, sessionColumns))

		assert.Len(t, blocks["pg_locks"].rows(t, lockColumns), 17,
			"the locks read beside it is unaffected - and on a wedged server this is the more "+
				"likely half to time out, which is why one refused read costs its own block")
	})

	t.Run("the locks read alone", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.locks = repeat(errResult(timedOut))

		blocks := capacityBlocks(t, takeSessionsSample(t, conn))

		assert.Contains(t, blocks["pg_locks"].header, "error=")
		assert.NotContains(t, blocks["pg_locks"].header, "locks_written")
		assert.Empty(t, blocks["pg_locks"].rows(t, lockColumns))

		assert.Len(t, blocks["pg_stat_activity"].rows(t, sessionColumns), 4)
	})

	t.Run("both, which is still one sample and no error", func(t *testing.T) {
		conn := newFakeSessionsConn()
		conn.sessions = repeat(errResult(denied))
		conn.locks = repeat(errResult(denied))

		blocks := capacityBlocks(t, takeSessionsSample(t, conn))
		require.Len(t, blocks, 2)

		for _, source := range []string{"pg_stat_activity", "pg_locks"} {
			assert.Contains(t, blocks[source].header, "error=", source)
		}
	})
}

func TestSessionsFailedReadsAreStillACompleteSample(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = repeat(errResult(errors.New("ERROR: permission denied")))
	conn.locks = repeat(errResult(errors.New("ERROR: permission denied")))

	results := runSessionsWindow(t, sessionsGoldenClock(t), connectTo(conn))

	assert.Equal(t, StatusComplete, results[0].Status,
		"a sample of degraded blocks is a sample: the window's stub is for a collector that "+
			"cannot localise a failure, and this one always can")
	assert.Equal(t, 3, results[0].SamplesWritten)
	assert.NotContains(t, artifactText(t, results[0]), "sample_error=", "so no stub was written")
}

func TestSessionsSampleErrorsOnlyOnAWriteFailure(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	err := Sessions{}.Sample(context.Background(), newFakeSessionsConn(),
		failingWriter{err: sinkErr}, sessionsSampleContext())

	require.ErrorIs(t, err, sinkErr, "which the window turns into IOErr rather than into a stub")
}

func TestSessionsGoldenFull(t *testing.T) {
	results := runSessionsWindow(t, sessionsGoldenClock(t), connectTo(newFakeSessionsConn()))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, 3, results[0].SamplesWritten)
	assert.Equal(t, bloatGolden(t, "pg_sessions_full.txt"), artifactText(t, results[0]))
}

func TestSessionsGoldenLeastPrivilege(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = queue(
		rowsResult(ordersMasked(at(32, 5, 61))),
		rowsResult(ordersMasked(at(32, 7, 55))),
		rowsResult(ordersMasked(at(32, 9, 52))),
	)

	results := runSessionsWindow(t, sessionsGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"complete, with no error= anywhere: a least-privilege capture of this artifact is "+
			"silent, and this file is what the server team has to read to see it")
	assert.Equal(t, bloatGolden(t, "pg_sessions_least_privilege.txt"), artifactText(t, results[0]))
}

func TestSessionsGoldenIdle(t *testing.T) {
	conn := newFakeSessionsConn()
	conn.sessions = queue(
		rowsResult(ordersIdleSessions(at(32, 5, 61))),
		rowsResult(ordersIdleSessions(at(32, 7, 55))),
		rowsResult(ordersIdleSessions(at(32, 9, 52))),
	)
	conn.locks = repeat(rowsResult(ordersIdleLocks()))

	results := runSessionsWindow(t, sessionsGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status,
		"no contention anywhere is a complete capture that found nothing, and it is the shape "+
			"most captures produce")

	artifact := artifactText(t, results[0])
	assert.NotContains(t, artifact, ",false,false,",
		"no ungranted lock row: nothing is waiting for anything")
	assert.Equal(t, bloatGolden(t, "pg_sessions_idle.txt"), artifact)
}

func TestSessionsGoldenQueryText(t *testing.T) {
	conn := newFakeQueryTextConn()

	results := runSessionsWindow(t, sessionsGoldenClock(t), connectTo(conn))

	require.Equal(t, StatusComplete, results[0].Status)
	assert.Equal(t, bloatGolden(t, "pg_sessions_query_text.txt"), artifactText(t, results[0]))
}
