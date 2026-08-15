package postgres

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// measured on postgres:18, 2026-08-15.
const (
	measuredStatementTimeout = "2026-08-15 10:01:40.031 UTC [25993] ERROR:  canceling statement due to statement timeout\n" +
		"2026-08-15 10:01:40.031 UTC [25993] STATEMENT:  SET statement_timeout='300ms'; SELECT pg_sleep(2);\n"

	measuredLockTimeout = "2026-08-15 10:01:41.480 UTC [26007] ERROR:  canceling statement due to lock timeout\n" +
		"2026-08-15 10:01:41.480 UTC [26007] CONTEXT:  while updating tuple (0,3) in relation \"yc_dl\"\n" +
		"2026-08-15 10:01:41.480 UTC [26007] STATEMENT:  SET lock_timeout='300ms'; UPDATE yc_dl SET v=8 WHERE id=1;\n"

	measuredIdleTimeout = "2026-08-15 10:00:41.302 UTC [7301] FATAL:  terminating connection due to idle-in-transaction timeout\n"
)

func TestTimeoutsArtifact(t *testing.T) {
	artifact := NewTimeouts().Artifact()

	assert.Equal(t, "pg_timeouts", artifact.Name)
	assert.Equal(t, "pg_timeouts.txt", artifact.FileName)
	assert.Equal(t, "cluster", artifact.Scope)
	assert.Equal(t, formatText, artifact.Format)
	assert.Equal(t, Every(DefaultLogTailInterval), artifact.Schedule)

	var _ Collector = NewTimeouts()
	var _ Epilogue = NewTimeouts()
}

func TestTimeoutsCapturesAllThreeTypesAtTheirMeasuredLineCounts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		fixture string
		lines   int
	}{
		{name: "statement timeout", fixture: measuredStatementTimeout, lines: 2},
		{name: "lock timeout", fixture: measuredLockTimeout, lines: 3},
		{name: "idle in transaction", fixture: measuredIdleTimeout, lines: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body, _, _, matched, _ := matchOnce(logFormatStderr, timeoutMatch, tt.fixture+unrelatedTraffic)

			require.Equal(t, 1, matched)
			assert.Equal(t, tt.fixture, body)
			assert.Equal(t, tt.lines, strings.Count(body, "\n"))
		})
	}
}

func TestTimeoutsIdleInTransactionFatalDoesNotAbsorbTheNextEventsStatement(t *testing.T) {
	next := "2026-08-15 10:00:42.000 UTC [7455] LOG:  duration: 1200.522 ms\n" +
		"2026-08-15 10:00:42.000 UTC [7455] STATEMENT:  SELECT * FROM order_items WHERE order_id = 9910\n"

	body, _, _, matched, _ := matchOnce(logFormatStderr, timeoutMatch, measuredIdleTimeout+next)

	require.Equal(t, 1, matched)
	assert.Equal(t, measuredIdleTimeout, body)
	assert.NotContains(t, body, "order_items",
		"the FATAL is one line, and the statement below it belongs to somebody else")
}

func TestTimeoutsLockTimeoutKeepsItsContextLine(t *testing.T) {
	body, _, _, matched, _ := matchOnce(logFormatStderr, timeoutMatch, measuredLockTimeout+unrelatedTraffic)

	require.Equal(t, 1, matched)
	assert.Contains(t, body, `CONTEXT:  while updating tuple (0,3) in relation "yc_dl"`,
		"CONTEXT names the relation and the tuple, which is one of the two lines a DBA reads first")
}

func timeoutCSV(severity, sqlstate, message string) string {
	return `2026-08-15 10:01:40.031 UTC,"postgres","postgres",25993,"[local]",6a803945.70,1,"SELECT",` +
		`2026-08-15 10:01:30.000 UTC,3/12,0,` + severity + `,` + sqlstate + `,"` + message + `",,,,,,,,,` +
		`"psql","client backend",,0` + "\n"
}

func TestTimeoutsAreMatchedBySQLStateInTheStructuredFormats(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sqlstate string
		message  string
	}{
		{name: "statement timeout", sqlstate: "57014", message: "canceling statement due to statement timeout"},
		{name: "lock timeout", sqlstate: "55P03", message: "canceling statement due to lock timeout"},
		{name: "idle in transaction", sqlstate: "25P03", message: "terminating connection due to idle-in-transaction timeout"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			record := timeoutCSV("ERROR", tt.sqlstate, tt.message)

			body, _, _, matched, _ := matchOnce(logFormatCSV, timeoutMatch, record+unrelatedCSV)

			require.Equal(t, 1, matched)
			assert.Equal(t, record, body)
		})
	}
}

func TestTimeoutsSharedSQLStatesArePairedWithTheirMessage(t *testing.T) {
	for _, tt := range []struct {
		name     string
		sqlstate string
		message  string
	}{
		{
			name:     "a client cancellation is not a statement timeout",
			sqlstate: "57014",
			message:  "canceling statement due to user request",
		},
		{
			name:     "SELECT ... FOR UPDATE NOWAIT is not a lock timeout",
			sqlstate: "55P03",
			message:  `could not obtain lock on row in relation ""yc_dl""`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, matched, _ := matchOnce(logFormatCSV, timeoutMatch,
				timeoutCSV("ERROR", tt.sqlstate, tt.message)+unrelatedCSV)

			assert.Zero(t, matched)
		})
	}

	assert.True(t, timeoutMatch.matches("25P03", "terminating connection due to idle-in-transaction timeout"))
	assert.True(t, deadlockMatch.matches("40P01", "irgendetwas auf Deutsch"),
		"40P01 and 25P03 stand alone, so the structured formats stay locale-independent for them")
}

var timeoutsGoldenWrites = []string{
	measuredStatementTimeout + unrelatedTraffic,
	measuredLockTimeout + unrelatedTraffic,
	measuredIdleTimeout + unrelatedTraffic,
}

func TestTimeoutsGoldenFull(t *testing.T) {
	results := runLogGoldenWindow(t, NewTimeouts(), logFormatStderr,
		priorTraffic, timeoutsGoldenWrites, 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "STATEMENT:  SELECT * FROM order_items",
		"nothing is invented for the idle-in-transaction FATAL, which is one line")

	assert.Equal(t, bloatGolden(t, "pg_timeouts_full.txt"), artifact)
}

func TestTimeoutsGoldenUnreadable(t *testing.T) {
	results := runRemoteGoldenWindow(t, NewTimeouts(), 30*time.Second, logGoldenClock(t, 3))

	require.Equal(t, StatusComplete, results[0].Status)

	artifact := artifactText(t, results[0])

	assert.NotContains(t, artifact, "matched=",
		"this is the artifact direction §3 wrote its sentence about: nothing anywhere counts "+
			"timeouts, so a zero here would be the report's only number and it would be invented")

	assert.Equal(t, bloatGolden(t, "pg_timeouts_unreadable.txt"), artifact)
}
