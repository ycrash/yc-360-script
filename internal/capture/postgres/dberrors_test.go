package postgres

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errorTail runs errorMatch through the engine as a tail collector does, so a test can
// read what it matched from real log files with the engine's block headers beside it.
type errorTail struct{ tail logTail }

func newErrorTail() *errorTail { return &errorTail{tail: newLogTail("pg_errors", errorMatch)} }

func (e *errorTail) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	return e.tail.sample(ctx, q, w, s)
}

func (e *errorTail) WriteClosing(w io.Writer, s SampleContext) error {
	return e.tail.writeClosing(w, s)
}

// measured on postgres:18, 2026-09-27, one session per event, with log_destination set to
// stderr,csvlog,jsonlog so the three files hold the same events.
const (
	measuredUniqueViolation = "2026-09-27 01:49:46.564 UTC [12395] ERROR:  duplicate key value violates unique constraint \"yc_errs_orders_pkey\"\n" +
		"2026-09-27 01:49:46.564 UTC [12395] DETAIL:  Key (id)=(4021) already exists.\n" +
		"2026-09-27 01:49:46.564 UTC [12395] STATEMENT:  INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');\n"

	measuredWarning = "2026-09-27 01:49:46.565 UTC [12395] WARNING:  yc-360 errors matcher sample warning\n" +
		"2026-09-27 01:49:46.565 UTC [12395] CONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n"

	measuredTimeoutBeside = "2026-09-27 01:49:46.870 UTC [12395] ERROR:  canceling statement due to statement timeout\n" +
		"2026-09-27 01:49:46.870 UTC [12395] STATEMENT:  SELECT pg_sleep(2);\n"

	measuredMissingDatabase = "2026-09-27 01:49:46.933 UTC [12402] FATAL:  database \"yc_no_such_database\" does not exist\n"

	measuredUserCancel = "2026-09-27 01:50:00.075 UTC [12463] ERROR:  canceling statement due to user request\n" +
		"2026-09-27 01:50:00.075 UTC [12463] STATEMENT:  SELECT pg_sleep(5)\n"

	measuredNowait = "2026-09-27 01:50:01.159 UTC [12492] ERROR:  could not obtain lock on row in relation \"yc_errs_lock\"\n" +
		"2026-09-27 01:50:01.159 UTC [12492] STATEMENT:  SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT\n"

	measuredReload = "2026-09-27 01:50:03.232 UTC [1] LOG:  received SIGHUP, reloading configuration files\n" +
		"2026-09-27 01:50:03.232 UTC [1] LOG:  parameter \"log_destination\" removed from configuration file, reset to default\n"
)

const (
	measuredUniqueViolationCSV = `2026-09-27 01:49:46.564 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,1,"INSERT",2026-09-27 01:49:46 UTC,21/620,2077,ERROR,23505,"duplicate key value violates unique constraint ""yc_errs_orders_pkey""","Key (id)=(4021) already exists.",,,,,"INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');",,,"psql","client backend",,8939275448745717619` + "\n"

	measuredWarningCSV = `2026-09-27 01:49:46.565 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,2,"DO",2026-09-27 01:49:46 UTC,21/621,0,WARNING,01000,"yc-360 errors matcher sample warning",,,,,"PL/pgSQL function inline_code_block line 1 at RAISE",,,,"psql","client backend",,6083687453455777963` + "\n"

	measuredTimeoutBesideCSV = `2026-09-27 01:49:46.870 UTC,"postgres","postgres",12395,"[local]",6ab8763a.306b,3,"SELECT",2026-09-27 01:49:46 UTC,21/623,0,ERROR,57014,"canceling statement due to statement timeout",,,,,,"SELECT pg_sleep(2);",,,"psql","client backend",,-8886085649470066503` + "\n"

	measuredMissingDatabaseCSV = `2026-09-27 01:49:46.933 UTC,"postgres","yc_no_such_database",12402,"[local]",6ab8763a.3072,1,"startup",2026-09-27 01:49:46 UTC,24/444,0,FATAL,3D000,"database ""yc_no_such_database"" does not exist",,,,,,,,,"","client backend",,0` + "\n"

	measuredUserCancelCSV = `2026-09-27 01:50:00.075 UTC,"postgres","postgres",12463,"[local]",6ab87647.30af,1,"SELECT",2026-09-27 01:49:59 UTC,12/718,0,ERROR,57014,"canceling statement due to user request",,,,,,"SELECT pg_sleep(5)",,,"yc_errs_cancel","client backend",,-8886085649470066503` + "\n"

	measuredNowaitCSV = `2026-09-27 01:50:01.159 UTC,"postgres","postgres",12492,"[local]",6ab87649.30cc,1,"SELECT",2026-09-27 01:50:01 UTC,53/414,0,ERROR,55P03,"could not obtain lock on row in relation ""yc_errs_lock""",,,,,,"SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT",,,"psql","client backend",,1217037425451197254` + "\n"

	measuredReloadCSV = `2026-09-27 01:50:03.232 UTC,,,1,,6ab73ece.1,50,,2026-09-26 03:41:02 UTC,,0,LOG,00000,"received SIGHUP, reloading configuration files",,,,,,,,,"","postmaster",,0` + "\n"
)

const (
	measuredUniqueViolationJSON = `{"timestamp":"2026-09-27 01:49:46.564 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":1,"ps":"INSERT","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/620","txid":2077,"error_severity":"ERROR","state_code":"23505","message":"duplicate key value violates unique constraint \"yc_errs_orders_pkey\"","detail":"Key (id)=(4021) already exists.","statement":"INSERT INTO yc_errs_orders (id, status) VALUES (4021, 'pending');","application_name":"psql","backend_type":"client backend","query_id":8939275448745717619}` + "\n"

	measuredWarningJSON = `{"timestamp":"2026-09-27 01:49:46.565 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":2,"ps":"DO","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/621","txid":0,"error_severity":"WARNING","state_code":"01000","message":"yc-360 errors matcher sample warning","context":"PL/pgSQL function inline_code_block line 1 at RAISE","application_name":"psql","backend_type":"client backend","query_id":6083687453455777963}` + "\n"

	measuredTimeoutBesideJSON = `{"timestamp":"2026-09-27 01:49:46.870 UTC","user":"postgres","dbname":"postgres","pid":12395,"remote_host":"[local]","session_id":"6ab8763a.306b","line_num":3,"ps":"SELECT","session_start":"2026-09-27 01:49:46 UTC","vxid":"21/623","txid":0,"error_severity":"ERROR","state_code":"57014","message":"canceling statement due to statement timeout","statement":"SELECT pg_sleep(2);","application_name":"psql","backend_type":"client backend","query_id":-8886085649470066503}` + "\n"

	measuredMissingDatabaseJSON = `{"timestamp":"2026-09-27 01:49:46.933 UTC","user":"postgres","dbname":"yc_no_such_database","pid":12402,"remote_host":"[local]","session_id":"6ab8763a.3072","line_num":1,"ps":"startup","session_start":"2026-09-27 01:49:46 UTC","vxid":"24/444","txid":0,"error_severity":"FATAL","state_code":"3D000","message":"database \"yc_no_such_database\" does not exist","backend_type":"client backend","query_id":0}` + "\n"

	measuredUserCancelJSON = `{"timestamp":"2026-09-27 01:50:00.075 UTC","user":"postgres","dbname":"postgres","pid":12463,"remote_host":"[local]","session_id":"6ab87647.30af","line_num":1,"ps":"SELECT","session_start":"2026-09-27 01:49:59 UTC","vxid":"12/718","txid":0,"error_severity":"ERROR","state_code":"57014","message":"canceling statement due to user request","statement":"SELECT pg_sleep(5)","application_name":"yc_errs_cancel","backend_type":"client backend","query_id":-8886085649470066503}` + "\n"

	measuredNowaitJSON = `{"timestamp":"2026-09-27 01:50:01.159 UTC","user":"postgres","dbname":"postgres","pid":12492,"remote_host":"[local]","session_id":"6ab87649.30cc","line_num":1,"ps":"SELECT","session_start":"2026-09-27 01:50:01 UTC","vxid":"53/414","txid":0,"error_severity":"ERROR","state_code":"55P03","message":"could not obtain lock on row in relation \"yc_errs_lock\"","statement":"SELECT id FROM yc_errs_lock WHERE id = 1 FOR UPDATE NOWAIT","application_name":"psql","backend_type":"client backend","query_id":1217037425451197254}` + "\n"

	measuredReloadJSON = `{"timestamp":"2026-09-27 01:50:03.232 UTC","pid":1,"session_id":"6ab73ece.1","line_num":50,"session_start":"2026-09-26 03:41:02 UTC","txid":0,"error_severity":"LOG","message":"received SIGHUP, reloading configuration files","backend_type":"postmaster","query_id":0}` + "\n"
)

// errorStream is the measured file in each format: what the errors tail takes, and all of it.
type errorStream struct {
	format logFormat
	log    string
	taken  string
}

func errorStreams() []errorStream {
	return []errorStream{
		{
			format: logFormatStderr,
			log: measuredUniqueViolation + measuredWarning + measuredTimeoutBeside + measuredMissingDatabase +
				measuredUserCancel + measuredNowait + measuredReload,
			taken: measuredUniqueViolation + measuredMissingDatabase + measuredUserCancel + measuredNowait,
		},
		{
			format: logFormatCSV,
			log: measuredUniqueViolationCSV + measuredWarningCSV + measuredTimeoutBesideCSV + measuredMissingDatabaseCSV +
				measuredUserCancelCSV + measuredNowaitCSV + measuredReloadCSV,
			taken: measuredUniqueViolationCSV + measuredMissingDatabaseCSV + measuredUserCancelCSV + measuredNowaitCSV,
		},
		{
			format: logFormatJSON,
			log: measuredUniqueViolationJSON + measuredWarningJSON + measuredTimeoutBesideJSON + measuredMissingDatabaseJSON +
				measuredUserCancelJSON + measuredNowaitJSON + measuredReloadJSON,
			taken: measuredUniqueViolationJSON + measuredMissingDatabaseJSON + measuredUserCancelJSON + measuredNowaitJSON,
		},
	}
}

// ended follows a stderr entry with one that proves where it ends: until then the tail holds
// the entry back, since another continuation line could still arrive.
func ended(format logFormat, log string) string {
	if format == logFormatStderr {
		return log + unrelatedTraffic
	}

	return log
}

func TestErrorMatchIsTheThreeLevelsLessTheOtherTwoTails(t *testing.T) {
	assert.Equal(t, []string{"ERROR", "FATAL", "PANIC"}, errorMatch.severity)
	require.Len(t, errorMatch.exclude, 2)
	assert.Equal(t, deadlockMatch, errorMatch.exclude[0])
	assert.Equal(t, timeoutMatch, errorMatch.exclude[1])
}

func TestErrorMatchTakesEveryErrorAndFatalEntryVerbatim(t *testing.T) {
	for _, stream := range errorStreams() {
		t.Run(string(stream.format), func(t *testing.T) {
			body, matched := matchBody(stream.format, errorMatch, stream.log)

			assert.Equal(t, 4, matched,
				"the unique violation, the missing database, the client's cancel and the NOWAIT "+
					"refusal: a cancel and a NOWAIT share their codes with the timeouts, "+
					"which is why the timeouts tail pairs those codes with a message")
			assert.Equal(t, stream.taken, body,
				"in the file's own format and order, the statement timeout left to its own tail "+
					"and the WARNING and LOG lines to nobody")
		})
	}
}

func TestErrorMatchCopiesTheWholeEvent(t *testing.T) {
	body, matched := matchBody(logFormatStderr, errorMatch, measuredUniqueViolation+measuredReload)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredUniqueViolation, body)
	assert.Contains(t, body, "DETAIL:  Key (id)=(4021) already exists.")
	assert.Contains(t, body, "STATEMENT:  INSERT INTO yc_errs_orders",
		"the lines that follow the ERROR belong to it, as they do in pg_deadlocks.txt")
}

func TestErrorMatchLeavesDeadlocksAndTimeoutsToTheirOwnTails(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format logFormat
		log    string
	}{
		{name: "stderr deadlock", format: logFormatStderr, log: measuredDeadlock},
		{name: "stderr statement timeout", format: logFormatStderr, log: measuredStatementTimeout},
		{name: "stderr lock timeout", format: logFormatStderr, log: measuredLockTimeout},
		{name: "stderr idle-in-transaction timeout", format: logFormatStderr, log: measuredIdleTimeout},
		{name: "csvlog deadlock", format: logFormatCSV, log: measuredDeadlockCSV},
		{
			name: "csvlog statement timeout", format: logFormatCSV,
			log: timeoutCSV("ERROR", "57014", "canceling statement due to statement timeout"),
		},
		{
			name: "csvlog lock timeout", format: logFormatCSV,
			log: timeoutCSV("ERROR", "55P03", "canceling statement due to lock timeout"),
		},
		{
			name: "csvlog idle-in-transaction timeout", format: logFormatCSV,
			log: timeoutCSV("FATAL", "25P03", "terminating connection due to idle-in-transaction timeout"),
		},
		{name: "jsonlog deadlock", format: logFormatJSON, log: measuredDeadlockJSON},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := ended(tc.format, tc.log)

			_, own := matchBody(tc.format, deadlockMatch, log)
			_, timeouts := matchBody(tc.format, timeoutMatch, log)
			require.Equal(t, 1, own+timeouts, "the entry is its own tail's")

			_, matched := matchBody(tc.format, errorMatch, log)
			assert.Zero(t, matched, "so it is not written a second time")
		})
	}
}

// Every entry at ERROR, FATAL or PANIC is copied by exactly one of the three tails, and
// nothing below ERROR by the errors tail: none is lost between them, none written twice.
func TestEveryErrorEntryIsCopiedByExactlyOneTail(t *testing.T) {
	type entry struct {
		format  logFormat
		log     string
		isError bool
	}

	entries := []entry{
		{logFormatStderr, measuredUniqueViolation, true},
		{logFormatStderr, measuredWarning, false},
		{logFormatStderr, measuredTimeoutBeside, true},
		{logFormatStderr, measuredMissingDatabase, true},
		{logFormatStderr, measuredUserCancel, true},
		{logFormatStderr, measuredNowait, true},
		{logFormatStderr, measuredReload, false},
		{logFormatStderr, measuredDeadlock, true},
		{logFormatStderr, measuredLockTimeout, true},
		{logFormatStderr, measuredIdleTimeout, true},
		{logFormatStderr, unrelatedTraffic, false},
		{logFormatCSV, measuredUniqueViolationCSV, true},
		{logFormatCSV, measuredWarningCSV, false},
		{logFormatCSV, measuredTimeoutBesideCSV, true},
		{logFormatCSV, measuredMissingDatabaseCSV, true},
		{logFormatCSV, measuredUserCancelCSV, true},
		{logFormatCSV, measuredNowaitCSV, true},
		{logFormatCSV, measuredReloadCSV, false},
		{logFormatCSV, measuredDeadlockCSV, true},
		{logFormatCSV, unrelatedCSV, false},
		{logFormatJSON, measuredUniqueViolationJSON, true},
		{logFormatJSON, measuredWarningJSON, false},
		{logFormatJSON, measuredTimeoutBesideJSON, true},
		{logFormatJSON, measuredMissingDatabaseJSON, true},
		{logFormatJSON, measuredUserCancelJSON, true},
		{logFormatJSON, measuredNowaitJSON, true},
		{logFormatJSON, measuredReloadJSON, false},
		{logFormatJSON, measuredDeadlockJSON, true},
		{logFormatJSON, unrelatedJSON, false},
	}

	for i, e := range entries {
		first, _, _ := strings.Cut(e.log, "\n")

		var copiedBy []string

		for name, m := range map[string]eventMatch{
			"pg_deadlocks": deadlockMatch, "pg_timeouts": timeoutMatch, "errors": errorMatch,
		} {
			if _, matched := matchBody(e.format, m, ended(e.format, e.log)); matched > 0 {
				copiedBy = append(copiedBy, name)
			}
		}

		if e.isError {
			assert.Len(t, copiedBy, 1, "entry %d (%s) %s", i, e.format, first)
		} else {
			assert.Empty(t, copiedBy, "entry %d (%s) %s", i, e.format, first)
		}
	}
}

// Constructed, not measured: a PANIC takes the server down, and the matrix servers are shared.
func TestErrorMatchTakesAPanic(t *testing.T) {
	panicLine := "2026-09-27 01:51:00.000 UTC [31] PANIC:  could not write to file \"pg_wal/xlogtemp.31\": No space left on device\n"

	body, matched := matchBody(logFormatStderr, errorMatch, panicLine+measuredReload)
	require.Equal(t, 1, matched)
	assert.Equal(t, panicLine, body)

	record := strings.Replace(measuredMissingDatabaseCSV, ",FATAL,3D000,", ",PANIC,53100,", 1)
	_, matched = matchBody(logFormatCSV, errorMatch, record)
	assert.Equal(t, 1, matched)

	line := strings.Replace(measuredMissingDatabaseJSON, `"error_severity":"FATAL"`, `"error_severity":"PANIC"`, 1)
	_, matched = matchBody(logFormatJSON, errorMatch, line)
	assert.Equal(t, 1, matched)
}

// Constructed: the German catalogue's word for ERROR. A translated level is not taken, as no
// tail takes a translated keyword; matched=0 there under-reports rather than mis-bounds.
func TestErrorMatchReadsTheEnglishLevelsOnly(t *testing.T) {
	translated := strings.Replace(measuredUniqueViolation, "] ERROR:  ", "] FEHLER:  ", 1)

	_, matched := matchBody(logFormatStderr, errorMatch, translated)
	assert.Zero(t, matched)

	_, matched = matchBody(logFormatCSV, errorMatch,
		strings.Replace(measuredUniqueViolationCSV, ",ERROR,23505,", ",FEHLER,23505,", 1))
	assert.Zero(t, matched)
}

func TestStderrSeverityNamesTheLevel(t *testing.T) {
	for _, tc := range []struct {
		line, severity string
	}{
		{measuredMissingDatabase, "FATAL"},
		{measuredUserCancel, "ERROR"},
		{measuredWarning, "WARNING"},
		{measuredReload, "LOG"},
		{"2026-09-27 01:50:00.075 UTC [12463] STATEMENT:  SELECT pg_sleep(5)", ""},
	} {
		first, _, _ := strings.Cut(tc.line, "\n")

		at, severity, _ := stderrSeverity(first)

		assert.Equal(t, tc.severity, severity, first)
		assert.Equal(t, tc.severity == "", at < 0, first)
	}
}

// runDBErrorsWindow runs d over a log file in format, the writes landing one per tick
// just after d's sample, so each is read by the next sample or by the drain.
func runDBErrorsWindow(t *testing.T, d *DBErrors, format logFormat, writes []string) ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	require.NoError(t, os.Mkdir("log", 0o755))

	name := "postgresql-2026-09-27_014945" + formatExtension(format)
	path := filepath.Join("log", name)

	require.NoError(t, os.WriteFile(path, []byte(priorTraffic), 0o644))
	require.NoError(t, os.WriteFile("current_logfiles", []byte(string(format)+" log/"+name+"\n"), 0o644))

	settings := logSettings{
		dataDirectory:    ".",
		logDirectory:     "log",
		loggingCollector: "on",
		logDestination:   string(format),
		read:             true,
	}

	writer := newFakeCollector("pg_log_writer")
	writer.artifact.Schedule = Every(DefaultLogTailInterval)

	tick := 0
	writer.sample = func(context.Context, SampleContext, io.Writer) error {
		if tick < len(writes) {
			appendFile(t, path, writes[tick])
		}

		tick++

		return nil
	}

	clock := newFakeClock()

	window := &Window{
		Target:     testTarget(),
		Duration:   time.Duration(len(writes)) * DefaultLogTailInterval,
		Collectors: []Collector{d, writer},
		now:        clock.now,
		after:      clock.after,
		connect: connectTo(&fakeLogConn{
			fakeWindowConn: newFakeWindowConn(),
			q:              deniedQuerier(settings),
		}),
	}

	results := window.Run(context.Background())
	results[1].File.Close()

	return results[0]
}

func dbErrorsFile(t *testing.T, result ArtifactResult) string {
	t.Helper()

	require.NoError(t, result.IOErr)
	result.File.Close()

	content, err := os.ReadFile(result.Artifact.FileName)
	require.NoError(t, err)

	return string(content)
}

func closingFieldMap(fields []headerField) map[string]string {
	out := map[string]string{}
	for _, f := range fields {
		out[f.key] = f.value
	}

	return out
}

func TestDBErrorsArtifact(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")
	artifact := d.Artifact()

	assert.Equal(t, "db_errors.log", DBErrorsLogName)
	assert.Equal(t, "pg_errors", artifact.Name)
	assert.Equal(t, "1.appLogs.db_errors.log", artifact.FileName, "the bundle's name, given by the caller")
	assert.Equal(t, "cluster", artifact.Scope)
	assert.Equal(t, Every(DefaultLogTailInterval), artifact.Schedule, "the other tails' 10s poll")
	assert.True(t, artifact.Bare, "no line in the file is the agent's")

	var _ Collector = d
	var _ Closing = d
}

func TestDBErrorsWritesTheServersEntriesAlone(t *testing.T) {
	for _, stream := range errorStreams() {
		t.Run(string(stream.format), func(t *testing.T) {
			d := NewDBErrors("1.appLogs.db_errors.log")

			result := runDBErrorsWindow(t, d, stream.format, []string{"", stream.log, ""})

			assert.Equal(t, stream.taken, dbErrorsFile(t, result),
				"the four entries in the server's own bytes and format, and not one line of the "+
					"agent's: no preamble, no block header, no closing block")
			assert.Equal(t, StatusComplete, result.Status)
			assert.True(t, d.LogRead())
			assert.Equal(t, 4, d.Kept())

			fields := closingFieldMap(d.closingFields())
			assert.Equal(t, map[string]string{
				"db_errors_log_access": "direct",
				"db_errors_log_format": string(stream.format),
				"db_errors_matched":    "4",
				"db_errors_dropped":    "0",
			}, fields, "the file cannot say which format its lines are in, so the account does")
		})
	}
}

func TestDBErrorsWritesAnEventHeldAcrossSamplesWhole(t *testing.T) {
	first, rest, _ := strings.Cut(measuredUniqueViolation, "STATEMENT:")
	cut := strings.LastIndex(first, "\n") + 1

	d := NewDBErrors("1.appLogs.db_errors.log")

	result := runDBErrorsWindow(t, d, logFormatStderr, []string{
		"", first[:cut], first[cut:] + "STATEMENT:" + rest + measuredReload,
	})

	assert.Equal(t, measuredUniqueViolation, dbErrorsFile(t, result),
		"a sample ending between the DETAIL and the STATEMENT holds the event, not writes half of it")
	assert.NotContains(t, closingFieldMap(d.closingFields()), "db_errors_partial_events")
}

func TestDBErrorsWritesAnEmptyFileWhenTheLogHadNoErrors(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")

	result := runDBErrorsWindow(t, d, logFormatStderr, []string{"", measuredWarning + measuredReload, ""})

	assert.Empty(t, dbErrorsFile(t, result))
	assert.True(t, d.LogRead(), "read, and nothing matched: an empty file rather than none")

	fields := closingFieldMap(d.closingFields())
	assert.Equal(t, "0", fields["db_errors_matched"], "a measured zero, beside direct access")
	assert.Equal(t, "direct", fields["db_errors_log_access"])
}

func TestDBErrorsWithoutTheLogWritesNothingAndSaysWhy(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")

	results := runRemoteGoldenWindow(t, d, 30*time.Second, logGoldenClock(t, 3))
	results[1].File.Close()

	assert.Empty(t, dbErrorsFile(t, results[0]))
	assert.False(t, d.LogRead(), "so the caller removes the file rather than upload an empty one")

	access, reason := d.LogAccess()
	assert.Equal(t, LogAccessNone, access)
	assert.Equal(t, reasonUnreadable, reason)

	assert.Equal(t, []headerField{
		{"db_errors_log_access", "none"},
		{"db_errors_log_access_reason", "unreadable"},
	}, d.closingFields(), "no count beside a log that was never read: absent, not 0")
}

func TestDBErrorsBeforeAnySampleIsUnknown(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")

	assert.Equal(t, []headerField{
		{"db_errors_log_access", "unknown"},
		{"db_errors_log_access_reason", "settings_unread"},
	}, d.closingFields(), "a refused connection never reached the log's settings")
	assert.False(t, d.LogRead())
}

func TestDBErrorsKeepsTheFirstEventsAndCountsTheRest(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")
	d.sampled, d.read = true, true
	d.written = MaxArtifactBytes - int64(len(measuredMissingDatabase))

	var buf strings.Builder

	require.NoError(t, d.write(&buf, &tailRead{
		events:  [][]byte{[]byte(measuredMissingDatabase), []byte(measuredUniqueViolation), []byte(measuredMissingDatabase)},
		matched: 3,
	}))

	assert.Equal(t, measuredMissingDatabase, buf.String(), "the first fits exactly")
	assert.Equal(t, 1, d.Kept())
	assert.Equal(t, 2, d.Dropped(),
		"the second does not fit, and the third, which would, is dropped too: the file keeps "+
			"the window's first errors, never a gap in the middle of them")

	fields := closingFieldMap(d.closingFields())
	assert.Equal(t, "3", fields["db_errors_matched"])
	assert.Equal(t, "2", fields["db_errors_dropped"])
}

func TestDBErrorsCountsEveryLimitItsReadsReached(t *testing.T) {
	d := NewDBErrors("1.appLogs.db_errors.log")
	d.sampled, d.read = true, true

	for _, read := range []*tailRead{
		{resolvedLate: true, rotated: true, skipped: 1024, scanTruncated: true},
		{rotated: true, truncated: true, carryDropped: true, eventsTruncated: 2},
		{partial: true, skipped: 2048, scanTruncated: true},
	} {
		require.NoError(t, d.write(io.Discard, read))
	}

	fields := closingFieldMap(d.closingFields())

	assert.Equal(t, "true", fields["db_errors_resolved_late"], "the window's start went unread")
	assert.Equal(t, "3072", fields["db_errors_skipped_bytes"], "past the per-sample read cap, summed")
	assert.Equal(t, "2", fields["db_errors_events_truncated"], "cut at 256 KB or 200 lines")
	assert.Equal(t, "2", fields["db_errors_rotations"])
	assert.Equal(t, "1", fields["db_errors_file_truncations"])
	assert.Equal(t, "1", fields["db_errors_carry_dropped"])
	assert.Equal(t, "1", fields["db_errors_partial_events"])
}
