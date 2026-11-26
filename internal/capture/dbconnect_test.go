package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yc-agent/internal/capture/dbcore"
	"yc-agent/internal/capture/postgres"
	"yc-agent/internal/config"
)

type logEntry struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// logEntries reads the JSON lines captureLogOutput collected.
func logEntries(t *testing.T, logs *bytes.Buffer) []logEntry {
	t.Helper()

	var entries []logEntry

	for line := range strings.SplitSeq(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}

		var entry logEntry
		require.NoError(t, json.Unmarshal([]byte(line), &entry), line)
		entries = append(entries, entry)
	}

	return entries
}

// entriesContaining filters entries by message text.
func entriesContaining(entries []logEntry, text string) []logEntry {
	var matched []logEntry

	for _, entry := range entries {
		if strings.Contains(entry.Message, text) {
			matched = append(matched, entry)
		}
	}

	return matched
}

var (
	errTestRefused = errors.New("refused")
	errTestSlots   = errors.New("no slot")
)

func describeTestFailure(err error) dbcore.ConnectFailure {
	switch {
	case errors.Is(err, errTestRefused):
		return dbcore.ConnectFailure{Cause: "nothing is accepting connections at db:5433", Fix: "Check port"}

	case errors.Is(err, errTestSlots):
		return dbcore.ConnectFailure{Cause: "the server has no free connection slot"}
	}

	return dbcore.ConnectFailure{Cause: "the connection failed"}
}

func refusedResult(err error, detail string) dbcore.ArtifactResult {
	return dbcore.ArtifactResult{Status: dbcore.StatusConnectFailed, ConnectErr: err, Err: detail}
}

func TestAConnectFailureOnEveryFileIsOneError(t *testing.T) {
	logs := captureLogOutput(t)

	logConnectFailures("PostgreSQL", []dbcore.ArtifactResult{
		refusedResult(errTestRefused, "dial tcp db:5433: connect: connection refused"),
		refusedResult(errTestRefused, "dial tcp db:5433: connect: connection refused"),
		refusedResult(errTestRefused, "dial tcp db:5433: connect: connection refused"),
	}, describeTestFailure)

	entries := logEntries(t, logs)
	require.Len(t, entries, 1, "one refusal, one line, however many files it emptied")
	assert.Equal(t, "error", entries[0].Level, "nothing was read from the database")
	assert.Equal(t, "PostgreSQL connection failed: nothing is accepting connections at db:5433. Check port. "+
		"No database data was captured. Details: dial tcp db:5433: connect: connection refused", entries[0].Message)
}

func TestAConnectFailureOnSomeFilesIsAWarning(t *testing.T) {
	logs := captureLogOutput(t)

	logConnectFailures("MySQL", []dbcore.ArtifactResult{
		refusedResult(errTestSlots, "Error 1040: Too many connections"),
		{Status: dbcore.StatusComplete},
		refusedResult(errTestSlots, "Error 1040: Too many connections"),
		{Status: dbcore.StatusPartial},
	}, describeTestFailure)

	entries := logEntries(t, logs)
	require.Len(t, entries, 1)
	assert.Equal(t, "warn", entries[0].Level, "the other connections captured their files")
	assert.Equal(t, "MySQL connection failed: the server has no free connection slot. "+
		"2 of 4 files were not captured; the rest were. Details: Error 1040: Too many connections", entries[0].Message,
		"no fix sentence when there is none to give")
}

func TestEachDistinctConnectFailureIsItsOwnLine(t *testing.T) {
	logs := captureLogOutput(t)

	logConnectFailures("PostgreSQL", []dbcore.ArtifactResult{
		refusedResult(errTestSlots, "first slot refusal"),
		refusedResult(errTestSlots, "second slot refusal"),
		refusedResult(errTestRefused, "refused"),
	}, describeTestFailure)

	entries := logEntries(t, logs)
	require.Len(t, entries, 2, "grouped by what the operator is told, not by the driver's text")
	assert.Contains(t, entries[0].Message, "no free connection slot")
	assert.Contains(t, entries[0].Message, "Details: first slot refusal")
	assert.Contains(t, entries[1].Message, "nothing is accepting connections")
}

func TestNoConnectFailureLogsNothing(t *testing.T) {
	logs := captureLogOutput(t)

	logConnectFailures("PostgreSQL", []dbcore.ArtifactResult{{Status: dbcore.StatusComplete}}, describeTestFailure)
	assert.Empty(t, logs.String())
}

func TestPostgresCaptureRunStatesTheRefusalBeforeTheHostLines(t *testing.T) {
	chdirToCaptureDir(t)
	logs := captureLogOutput(t)

	target := withWindow(t, time.Minute)

	_, err := (&PostgresCapture{Target: target}).Run()
	require.NoError(t, err)

	entries := logEntries(t, logs)
	failures := entriesContaining(entries, "connection failed")
	require.Len(t, failures, 1)
	assert.Equal(t, "error", failures[0].Level)

	address := "127.0.0.1:" + strconv.Itoa(target.Port)
	assert.True(t, strings.HasPrefix(failures[0].Message, "PostgreSQL connection failed: nothing is accepting "+
		"connections at "+address+". Check port and host in the postgres: block, and that the database is running. "+
		"No database data was captured. Details: failed to connect to `user=ycrash_monitor database=orders_db`"),
		failures[0].Message)
	assert.NotContains(t, failures[0].Message, pgTestPassword)

	hint := entriesContaining(entries, "Host capture: ")
	require.Len(t, hint, 1)
	assert.Equal(t, "Host capture: the agent could not connect to the database to check (see the connection error "+
		"above); if the database is down and runs on this machine, set agentOnDbHost: true in the postgres: block "+
		"to capture host data anyway", hint[0].Message)

	assert.Less(t, strings.Index(logs.String(), "connection failed"), strings.Index(logs.String(), "Host capture skipped"),
		"the cause comes before what it prevented")
}

func TestAnyConnectedNeedsOneFileWhoseConnectionOpened(t *testing.T) {
	refused := refusedResult(errTestSlots, "no slot")

	assert.False(t, anyConnected([]dbcore.ArtifactResult{refused, refused}))
	assert.True(t, anyConnected([]dbcore.ArtifactResult{refused, {Status: dbcore.StatusConnectionLost}}),
		"a connection lost mid-window had opened")
}

func TestTheHostHintAfterAPartialConnectDoesNotCallTheDatabaseDown(t *testing.T) {
	unconnected := postgres.Metadata{AgentOnDBHost: postgres.OnDBHostUnknown, AgentOnDBHostReason: "no_connection"}

	for _, tt := range []struct {
		connected bool
		hint      string
	}{
		{false, "Host capture: the agent could not connect to the database to check (see the connection error " +
			"above); if the database is down and runs on this machine, set agentOnDbHost: true in the postgres: " +
			"block to capture host data anyway"},
		{true, "Host capture: the connection this check runs on failed, though others opened (see the warning " +
			"above); run the capture again once it can connect, or, if the database runs on this machine, set " +
			"agentOnDbHost: true in the postgres: block to capture host data anyway"},
	} {
		logs := captureLogOutput(t)

		(&PostgresCapture{}).logHostCaptureSkipped(unconnected, tt.connected)

		hint := entriesContaining(logEntries(t, logs), "Host capture: ")
		require.Len(t, hint, 1)
		assert.Equal(t, tt.hint, hint[0].Message, "connected=%t", tt.connected)
	}
}

func TestMySQLCaptureRunStatesTheRefusal(t *testing.T) {
	chdirToCaptureDir(t)
	logs := captureLogOutput(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	window := config.Duration(time.Minute)
	target := &config.MySQL{DBTarget: config.DBTarget{
		ID:              "orders",
		Host:            "127.0.0.1",
		Port:            port,
		Username:        "yc_monitor",
		Password:        "s3cr3t-do-not-log",
		CaptureDuration: &window,
	}}

	_, err = (&MySQLCapture{Target: target}).Run()
	require.NoError(t, err)

	failures := entriesContaining(logEntries(t, logs), "connection failed")
	require.Len(t, failures, 1)
	assert.Equal(t, "error", failures[0].Level)
	assert.True(t, strings.HasPrefix(failures[0].Message, "MySQL connection failed: nothing is accepting connections at "+
		"127.0.0.1:"+strconv.Itoa(port)+". Check port and host in the mysql: block, and that the database is running. "+
		"No database data was captured. Details: "), failures[0].Message)
	assert.NotContains(t, failures[0].Message, "s3cr3t-do-not-log")
}
