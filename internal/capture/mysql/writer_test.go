package mysql

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestASampleBlockCarriesTheEnvelopeAndEveryRow(t *testing.T) {
	q := (&fakeQuerier{}).on("FROM performance_schema.threads", fakeResult{
		columns: []string{"THREAD_ID", "PROCESSLIST_INFO", "program_name"},
		rows: [][]any{
			{48, "UPDATE orders SET status = 'paid' WHERE id = 42", nil},
			{49, nil, ProgramName},
		},
	}).on("information_schema.innodb_trx", fakeResult{
		columns: []string{"trx_id", "trx_state", "trx_started", "trx_mysql_thread_id"},
	})

	var out bytes.Buffer

	collector := NewSessions(15 * time.Second)
	require.NoError(t, collector.Sample(context.Background(), q, &out, testSample(1, 2)))

	assert.Equal(t, `# capture_id=capture-1 target_id=orders-primary engine=mysql engine_version=8.4.3 v=1 format=csv db=orders_db
# sample_id=1 source=threads start_ts=2026-10-27T09:00:00.001Z end_ts=2026-10-27T09:00:00.002Z duration_ms=1 status=OK rows=2 truncated=false scope=cluster redacted=2 ts=2026-10-27T09:00:00.000Z
THREAD_ID,PROCESSLIST_INFO,program_name
48,UPDATE orders SET status = '<redacted>' WHERE id = <redacted>,
49,,yCrash-DB-Agent
# capture_id=capture-1 target_id=orders-primary engine=mysql engine_version=8.4.3 v=1 format=csv db=orders_db
# sample_id=1 source=innodb_trx start_ts=2026-10-27T09:00:00.003Z end_ts=2026-10-27T09:00:00.004Z duration_ms=1 status=OK rows=0 truncated=false scope=cluster ts=2026-10-27T09:00:00.000Z
trx_id,trx_state,trx_started,trx_mysql_thread_id
`, out.String())
}

func TestARefusedBlockIsAskedOnceAndRemembered(t *testing.T) {
	q := (&fakeQuerier{}).on("FROM performance_schema.threads", fakeResult{
		columns: []string{"THREAD_ID"}, rows: [][]any{{1}},
	}).on("information_schema.innodb_trx", fakeResult{
		err: serverError(errSpecificAccess, "Access denied; you need (at least one of) the PROCESS privilege(s)"),
	})

	collector := NewSessions(15 * time.Second)

	var first, second bytes.Buffer

	require.NoError(t, collector.Sample(context.Background(), q, &first, testSample(1, 2)))
	require.NoError(t, collector.Sample(context.Background(), q, &second, testSample(2, 2)))

	assert.Equal(t, 1, q.count("innodb_trx"), "each refusal counts against the server's own error statistics")
	assert.Contains(t, first.String(), "source=innodb_trx start_ts=2026-10-27T09:00:00.003Z")
	assert.Contains(t, first.String(), "status=OK rows=0 truncated=false scope=cluster reason=permission_denied error=")
	assert.Contains(t, second.String(), "source=innodb_trx start_ts= end_ts= duration_ms= status=OK rows=0 "+
		"truncated=false scope=cluster reason=permission_denied ts=",
		"no statement is run for a remembered refusal")
	assert.Contains(t, second.String(), "trx_id,trx_state,trx_started,trx_mysql_thread_id\n",
		"the declared columns stand in for the result")
}

func TestAFailedBlockKeepsTheOthersAndATimeoutSaysSo(t *testing.T) {
	q := (&fakeQuerier{}).on("FROM performance_schema.threads", fakeResult{
		err: serverError(errQueryTimeout, "Query execution was interrupted, maximum statement execution time exceeded"),
	}).on("information_schema.innodb_trx", fakeResult{
		columns: []string{"trx_id"}, rows: [][]any{{"421"}},
	})

	var out bytes.Buffer

	require.NoError(t, NewSessions(15*time.Second).Sample(context.Background(), q, &out, testSample(1, 2)))

	assert.Contains(t, out.String(), "source=threads start_ts=2026-10-27T09:00:00.001Z end_ts=2026-10-27T09:00:00.002Z "+
		"duration_ms=1 status=TIMEOUT rows=0")
	assert.Contains(t, out.String(), "THREAD_ID,PROCESSLIST_ID,")
	assert.Contains(t, out.String(), "trx_id\n421\n")
}

func TestASampleWhoseEveryBlockFailedIsTheWindowsStub(t *testing.T) {
	failure := serverError(1205, "Lock wait timeout exceeded")
	q := (&fakeQuerier{}).
		on("FROM performance_schema.threads", fakeResult{err: failure}).
		on("information_schema.innodb_trx", fakeResult{err: failure})

	var out bytes.Buffer

	err := NewSessions(15*time.Second).Sample(context.Background(), q, &out, testSample(1, 2))

	assert.ErrorIs(t, err, failure)
	assert.Empty(t, out.String(), "the window writes the stub; a sample that fails writes nothing")
}

func TestARowCapIsKnownNotGuessed(t *testing.T) {
	rows := make([][]any, DefaultMaxLocks+1)
	for i := range rows {
		rows[i] = []any{"INNODB", "a", "b", 1, "c", "d", 2}
	}

	q := (&fakeQuerier{}).on("data_lock_waits", fakeResult{
		columns: []string{"ENGINE", "A", "B", "C", "D", "E", "F"}, rows: rows,
	}).on("metadata_locks", fakeResult{columns: []string{"OBJECT_TYPE"}})

	var out bytes.Buffer

	require.NoError(t, NewLockWaits(15*time.Second).Sample(context.Background(), q, &out, testSample(1, 2)))

	assert.Contains(t, out.String(), "rows=5000 truncated=true")
	assert.Contains(t, q.sent[0], "LIMIT 5001", "one row past the cap is what says it was reached")
}

func TestATopHundredSaysHowManyThereWere(t *testing.T) {
	columns := []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "DATA_FREE", "rows_total"}

	tables := func(n, total int) [][]any {
		rows := make([][]any, n)
		for i := range rows {
			rows[i] = []any{"yc_bulk", "t", "InnoDB", 16384, 0, 0, total}
		}

		return rows
	}

	sample := func(rows [][]any) string {
		t.Helper()

		q := (&fakeQuerier{}).on("information_schema.TABLES", fakeResult{columns: columns, rows: rows})

		var out bytes.Buffer

		require.NoError(t, NewStorage(time.Hour).Sample(context.Background(), q, &out, testSample(1, 2)))
		assert.Contains(t, q.sent[0], "LIMIT 100", "the spec's LIMIT, kept")

		return out.String()
	}

	cut := sample(tables(100, 124))
	assert.Contains(t, cut, "rows=100 truncated=true scope=cluster rows_total=124 ")
	assert.Contains(t, cut, "\nTABLE_SCHEMA,TABLE_NAME,ENGINE,DATA_LENGTH,INDEX_LENGTH,DATA_FREE\n",
		"the total is the header's, not a column")
	assert.Contains(t, cut, "\nyc_bulk,t,InnoDB,16384,0,0\n")

	assert.Contains(t, sample(tables(100, 100)), "rows=100 truncated=false scope=cluster rows_total=100 ")
	assert.Contains(t, sample(nil), "rows=0 truncated=false scope=cluster rows_total=0 ")
}

func TestATextValueIsCappedAndAStatementCutMidLiteralIsStillReplaced(t *testing.T) {
	long := "SELECT '" + strings.Repeat("x", DefaultMaxText) + "'"

	text, cut, redacted := cell(long, redactStatement)

	assert.True(t, cut)
	assert.Equal(t, "SELECT '<redacted>...", text)
	assert.Equal(t, 1, redacted)
}
