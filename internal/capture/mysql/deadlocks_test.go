package mysql

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// verbosity2Report is a deadlock as log_error_verbosity=2 leaves it: InnoDB's dump
// with no note before it, measured on the matrix.
const verbosity2Report = `TRANSACTION 2575, ACTIVE 3 sec starting index read
mysql tables in use 1, locked 1
LOCK WAIT 3 lock struct(s), heap size 1128, 2 row lock(s), undo log entries 1
MySQL thread id 169, OS thread handle 2814, query id 318 127.0.0.1 root updating
UPDATE yc_app.orders SET status='a' WHERE id=2
RECORD LOCKS space id 2 page no 6 n bits 464 index PRIMARY of table ` + "`yc_app`.`orders`" + ` trx id 2575 lock_mode X
Record lock, heap no 2 PHYSICAL RECORD: n_fields 5; compact format; info bits 0
 4: len 1; hex 61; asc a;;

TRANSACTION 2576, ACTIVE 2 sec starting index read
MySQL thread id 171, OS thread handle 2815, query id 321 127.0.0.1 root updating
UPDATE yc_app.orders SET status='b' WHERE id=1
`

// verbosity3Report is the same at log_error_verbosity=3: the notes frame it.
const verbosity3Report = `2026-10-27T13:29:48.410257Z 0 [Note] [MY-012468] [InnoDB] Transactions deadlock detected, dumping detailed information.
2026-10-27T13:29:48.411828Z 0 [Note] [MY-012469] [InnoDB]  *** (1) TRANSACTION:
TRANSACTION 2582, ACTIVE 3 sec starting index read
MySQL thread id 793, OS thread handle 2814, query id 1586 127.0.0.1 root updating
UPDATE yc_app.orders SET status='a' WHERE id=2
2026-10-27T13:29:48.440892Z 0 [Note] [MY-012469] [InnoDB] *** WE ROLL BACK TRANSACTION (1)
`

const unrelatedEntry = "2026-10-27T13:30:00.000000Z 12 [Warning] [MY-013360] [Server] Plugin sha256_password reported: deprecated\n"

func deadlockLog(t *testing.T) (string, *fakeQuerier) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "error.log")
	require.NoError(t, os.WriteFile(path, []byte(unrelatedEntry), 0o600))

	q := (&fakeQuerier{}).on("SHOW GLOBAL VARIABLES", fakeResult{
		columns: []string{"Variable_name", "Value"},
		rows:    [][]any{{"log_error", path}, {"datadir", "/var/lib/mysql/"}, {"innodb_print_all_deadlocks", "ON"}},
	})

	return path, q
}

func appendLog(t *testing.T, path, text string) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)

	_, err = f.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

func TestDeadlocksReadsTheWindowsReportsInBothShapes(t *testing.T) {
	path, q := deadlockLog(t)
	d := &Deadlocks{Interval: time.Minute}

	var out bytes.Buffer

	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(1, 3)))
	assert.Contains(t, out.String(), "innodb_print_all_deadlocks=ON log_path="+path+" log_access=direct "+
		"from_offset=106 to_offset=106 matched=0", "opened at its end: what was logged before the window is not read")

	appendLog(t, path, verbosity2Report+unrelatedEntry)
	out.Reset()

	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(2, 3)))
	assert.Contains(t, out.String(), "matched=1 redacted=5")
	assert.Contains(t, out.String(), "UPDATE yc_app.orders SET status='<redacted>' WHERE id=<redacted>\n")
	assert.Contains(t, out.String(), " 4: len 1; hex <redacted>; asc <redacted>;;\n")
	assert.Contains(t, out.String(), "TRANSACTION 2576, ACTIVE 2 sec", "both transactions are one report")
	assert.NotContains(t, out.String(), "sha256_password", "only the report is copied")

	appendLog(t, path, verbosity3Report)
	out.Reset()

	require.NoError(t, d.WriteClosing(&out, testSample(3, 3)))
	assert.Contains(t, out.String(), "drain=true")
	assert.Contains(t, out.String(), "matched=1")
	assert.Contains(t, out.String(), "[MY-012468] [InnoDB] Transactions deadlock detected")
	assert.Contains(t, out.String(), "WE ROLL BACK TRANSACTION (1)")
}

func TestDeadlocksFollowsARotatedLog(t *testing.T) {
	path, q := deadlockLog(t)
	d := &Deadlocks{Interval: time.Minute}

	var out bytes.Buffer

	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(1, 3)))

	// The old file's last report, then a new file at the path, as FLUSH ERROR LOGS
	// after a rename leaves it.
	appendLog(t, path, verbosity2Report)
	require.NoError(t, os.Rename(path, path+".1"))
	require.NoError(t, os.WriteFile(path, []byte(verbosity3Report), 0o600))

	out.Reset()
	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(2, 3)))

	assert.Contains(t, out.String(), "rotated=true")
	assert.Contains(t, out.String(), "matched=2")
	assert.Contains(t, out.String(), "TRANSACTION 2575")
	assert.Contains(t, out.String(), "TRANSACTION 2582")
}

func TestDeadlocksNamesTheFallbackWhereTheLogCannotBeRead(t *testing.T) {
	q := (&fakeQuerier{}).on("SHOW GLOBAL VARIABLES", fakeResult{
		columns: []string{"Variable_name", "Value"},
		rows:    [][]any{{"log_error", "stderr"}, {"innodb_print_all_deadlocks", "OFF"}},
	})

	d := &Deadlocks{Interval: time.Minute}

	var out bytes.Buffer

	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Contains(t, out.String(), `innodb_print_all_deadlocks=OFF log_path= log_access=none reason=stderr `+
		`fallback="my_buffer_pool.txt:LATEST DETECTED DEADLOCK" bytes=0`)

	out.Reset()
	require.NoError(t, d.WriteClosing(&out, testSample(2, 2)))
	assert.Empty(t, out.String())
}

func TestDeadlocksNamesTheFallbackWhileTheServerLogsNoReports(t *testing.T) {
	path, _ := deadlockLog(t)

	q := (&fakeQuerier{}).on("SHOW GLOBAL VARIABLES", fakeResult{
		columns: []string{"Variable_name", "Value"},
		rows:    [][]any{{"log_error", path}, {"datadir", "/var/lib/mysql/"}, {"innodb_print_all_deadlocks", "OFF"}},
	})

	d := &Deadlocks{Interval: time.Minute}

	var out bytes.Buffer

	require.NoError(t, d.Sample(context.Background(), q, &out, testSample(1, 2)))
	assert.Contains(t, out.String(), `innodb_print_all_deadlocks=OFF fallback="my_buffer_pool.txt:LATEST DETECTED DEADLOCK" `+
		`log_path=`+path+` log_access=direct`)

	appendLog(t, path, verbosity3Report)
	out.Reset()

	require.NoError(t, d.WriteClosing(&out, testSample(2, 2)))
	assert.Contains(t, out.String(), "matched=1", "the log is still read: the setting can be turned on mid-window")
	assert.Contains(t, out.String(), `fallback="my_buffer_pool.txt:LATEST DETECTED DEADLOCK"`)
}

func TestDeadlocksAreReadAtTheLogTailCadence(t *testing.T) {
	for _, collector := range Collectors(NewMetadata(Target{}, "test", time.Now(), ""), "", 15*time.Second,
		5*time.Minute, 15*time.Minute) {
		if deadlocks, ok := collector.(*Deadlocks); ok {
			assert.Equal(t, LogTailInterval, deadlocks.Artifact().Schedule.Offsets(time.Minute)[1],
				"a tail read at the normal speed takes at most MaxScanBytes every five minutes")

			return
		}
	}

	t.Fatal("no my_deadlocks collector")
}
