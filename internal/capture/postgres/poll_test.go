package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testPollNow = time.Date(2026, 8, 25, 9, 12, 0, 0, time.UTC)

// confirmedPoll is a poll from the database's own machine: heartbeat, connections
// and disk, and no reason field because the verdict is yes.
func confirmedPoll() PollResult {
	return PollResult{
		TS:              testPollNow,
		Runner:          "db-prod-01",
		TargetHost:      "localhost",
		TargetPort:      5432,
		TargetDatabase:  "orders",
		AgentOnDBHost:   OnDBHostYes,
		AgentOnDBHostBy: confirmedByBackendPID,

		ServerVersionNum: "170006",

		ConnectMS: "4.8",
		QueryMS:   "0.7",

		CurrentConnections: "141",
		BackendsTotal:      "148",
		BackendsMasked:     "0",

		MaxConnections:               "200",
		SuperuserReservedConnections: "3",
		ReservedConnections:          "0",

		DiskFreeBytes:  "48318382080",
		DiskTotalBytes: "107374182400",
		DiskMount:      "/var/lib/postgresql/17/main/pg_wal",
	}
}

// jumpBoxPoll is a poll of a managed database from another machine: the target's
// numbers, the runner's health, and no host-scoped data at all.
func jumpBoxPoll() PollResult {
	return PollResult{
		TS:                  testPollNow,
		Runner:              "jump-01",
		TargetHost:          "orders.abc123.eu-west-1.rds.amazonaws.com",
		TargetPort:          5432,
		TargetDatabase:      "orders",
		AgentOnDBHost:       OnDBHostNo,
		AgentOnDBHostReason: hostReasonPIDAbsent,

		ServerVersionNum: "160010",

		ConnectMS: "38.4",
		QueryMS:   "1.9",

		CurrentConnections: "141",
		BackendsTotal:      "152",
		BackendsMasked:     "0",

		MaxConnections:               "450",
		SuperuserReservedConnections: "3",
		ReservedConnections:          "0",

		RunnerLoad1: "0.42",
		AgentCPUPct: "0.6",

		DiskReason: diskReasonNotCoResident,
	}
}

// downPoll is the reading that matters most: the database did not answer. The
// payload is still sent, and the reason rows say what could not be read.
func downPoll() PollResult {
	return PollResult{
		TS:                  testPollNow,
		Runner:              "db-prod-01",
		TargetHost:          "localhost",
		TargetPort:          5432,
		TargetDatabase:      "orders",
		AgentOnDBHost:       OnDBHostUnknown,
		AgentOnDBHostReason: hostReasonNoConnection,

		HeartbeatError: heartbeatUnreachable,

		RunnerLoad1: "7.9",
		AgentCPUPct: "0.4",

		DiskReason: diskReasonNoConnection,
	}
}

func renderPoll(t *testing.T, p PollResult) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, WritePoll(&buf, p))

	return buf.String()
}

func TestWritePoll(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result PollResult
		golden string
	}{
		{"on the database host", confirmedPoll(), "pg_m3_confirmed.txt"},
		{"a jump box polling a managed database", jumpBoxPoll(), "pg_m3_jump_box.txt"},
		{"the database is down", downPoll(), "pg_m3_down.txt"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, golden(t, tt.golden), renderPoll(t, tt.result))
		})
	}
}

func TestPollHeaderCarriesTheVerdictAndItsReason(t *testing.T) {
	t.Run("a yes carries which test produced it and no reason", func(t *testing.T) {
		header := renderPoll(t, confirmedPoll())

		assert.Contains(t, header, "agent_on_db_host=yes")
		assert.Contains(t, header, "agent_on_db_host_by=backend_pid")
		assert.NotContains(t, header, "agent_on_db_host_reason=")
	})

	// A bare no or unknown is a bug: the operator has nothing to act on.
	for _, verdict := range []PollResult{jumpBoxPoll(), downPoll()} {
		t.Run("a "+verdict.AgentOnDBHost+" carries a reason", func(t *testing.T) {
			assert.Contains(t, renderPoll(t, verdict),
				"agent_on_db_host_reason="+verdict.AgentOnDBHostReason)
		})
	}

	t.Run("an unread server version leaves the field out", func(t *testing.T) {
		assert.NotContains(t, renderPoll(t, downPoll()), "server_version_num=")
	})
}

func TestPollRowsLeaveOutWhatWasNotRead(t *testing.T) {
	// Zero would read as a healthy server with no connections.
	assert.NotContains(t, renderPoll(t, downPoll()), "current_connections")

	// PostgreSQL 14 and 15 have no reserved_connections; the row goes rather than
	// carrying a number the server never sent. superuser_reserved_connections shares
	// the suffix, so the whole row is the thing to look for.
	pre16 := confirmedPoll()
	pre16.ReservedConnections = ""

	assert.NotContains(t, renderPoll(t, pre16), "\nreserved_connections,")
}

func TestPollLogLine(t *testing.T) {
	assert.Equal(t, "pg_m3: target=localhost:5432/orders agent_on_db_host=yes sent=true",
		confirmedPoll().LogLine(true))

	assert.Equal(t, "pg_m3: target=localhost:5432/orders agent_on_db_host=unknown "+
		"reason=no_connection sent=false", downPoll().LogLine(false))
}

func TestPollDeclarationOnlyRaisesAnUnknown(t *testing.T) {
	t.Run("a down database is what the declaration exists for", func(t *testing.T) {
		result := downPoll()
		result.applyDeclaration(true)

		assert.Equal(t, OnDBHostYes, result.AgentOnDBHost)
		assert.Equal(t, confirmedByConfigured, result.AgentOnDBHostBy)
		assert.Empty(t, result.AgentOnDBHostReason)
		assert.False(t, result.DeclarationContradicted)
	})

	t.Run("a measured no beats the declaration and is recorded as a disagreement", func(t *testing.T) {
		result := jumpBoxPoll()
		result.applyDeclaration(true)

		assert.Equal(t, OnDBHostNo, result.AgentOnDBHost)
		assert.True(t, result.DeclarationContradicted)
	})

	t.Run("no declaration leaves the verdict alone", func(t *testing.T) {
		result := downPoll()
		result.applyDeclaration(false)

		assert.Equal(t, OnDBHostUnknown, result.AgentOnDBHost)
	})
}

func TestClassifyHeartbeat(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{"no failure", nil, ""},
		{"the server is at max_connections",
			fmt.Errorf("%w: %w", ErrTooManyConnections, &pgconn.PgError{Code: tooManyConnections}),
			heartbeatTooManyConnections},
		{"a rejected password", &pgconn.PgError{Code: "28P01"}, heartbeatAuthFailed},
		{"a role that may not connect", &pgconn.PgError{Code: "28000"}, heartbeatAuthFailed},
		{"the connect deadline passed", context.DeadlineExceeded, heartbeatTimeout},
		{"the name does not resolve", &net.DNSError{Err: "no such host"}, heartbeatUnreachable},
		{"the port refuses", &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			heartbeatUnreachable},
		{"the server answered and refused the statement",
			&pgconn.PgError{Code: "42501"}, heartbeatStatementFailed},
		{"anything else", errors.New("unexpected"), heartbeatOther},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifyHeartbeat(tt.err))
		})
	}

	// pgx joins one error per resolved address, so the SQLSTATE can be past the first.
	t.Run("a joined error is walked to its SQLSTATE", func(t *testing.T) {
		joined := errors.Join(errors.New("dial 10.0.0.1: refused"), &pgconn.PgError{Code: "28P01"})

		assert.Equal(t, heartbeatAuthFailed, classifyHeartbeat(joined))
	})
}

func TestReadPollRow(t *testing.T) {
	querier := &fakePollQuerier{row: fakeRow{values: pollRowValues()}}

	row, elapsed, err := readPollRow(context.Background(), querier)
	require.NoError(t, err)

	assert.Equal(t, "48211", int32Text(row.backendPID))
	assert.Equal(t, "141", int64Text(row.currentConnections))
	assert.Equal(t, "148", int64Text(row.backendsTotal))
	assert.Equal(t, "/var/lib/postgresql/17/main", text(row.dataDirectory))
	assert.GreaterOrEqual(t, elapsed, time.Duration(0))

	// The statement is bounded whatever the caller's deadline.
	assert.WithinDuration(t, time.Now().Add(StatementTimeout), querier.deadline, time.Second)
}

func TestPollAppliesTheStatement(t *testing.T) {
	var result PollResult

	row := pollRowFrom(t, pollRowValues())
	result.applyRow(row)

	assert.Equal(t, "141", result.CurrentConnections)
	assert.Equal(t, "148", result.BackendsTotal)
	assert.Equal(t, "0", result.BackendsMasked)
	assert.Equal(t, "200", result.MaxConnections)
	assert.Equal(t, "3", result.SuperuserReservedConnections)
	assert.Equal(t, "170006", result.ServerVersionNum)
}

// A LOGIN-only role sees backend_type as NULL for every session but its own, so
// current_connections counts the masked rows too - never lower than the truth.
func TestPollCountsMaskedRowsAsConnections(t *testing.T) {
	values := pollRowValues()
	values[7] = ptr(int64(152)) // current_connections: client backends plus masked
	values[8] = ptr(int64(11))  // backends_masked

	var result PollResult
	result.applyRow(pollRowFrom(t, values))

	assert.Equal(t, "152", result.CurrentConnections)
	assert.Equal(t, "11", result.BackendsMasked)
}

// The connection opened and the statement did not: a different fault from never
// having reached the server, and the payload has to tell them apart.
func TestPollStatementFailure(t *testing.T) {
	querier := &fakePollQuerier{row: fakeRow{err: &pgconn.PgError{Code: "42501"}}}

	result := PollResult{
		ConnectMS:           "4.8",
		AgentOnDBHost:       OnDBHostUnknown,
		AgentOnDBHostReason: hostReasonNoConnection,
	}
	result.collect(context.Background(), querier,
		PollRequest{Target: Target{Password: "hunter2"}})

	assert.Equal(t, heartbeatStatementFailed, result.HeartbeatError)
	assert.Equal(t, "4.8", result.ConnectMS, "the dial was measured and still counts")
	assert.Empty(t, result.QueryMS)
	assert.Empty(t, result.CurrentConnections, "an unread count is never a zero")

	assert.Equal(t, OnDBHostUnknown, result.AgentOnDBHost)
	assert.Equal(t, hostReasonBackendPIDUnread, result.AgentOnDBHostReason,
		"the connection opened, so no_connection is no longer the reason")

	assert.Equal(t, diskReasonDBHostUnknown, result.DiskReason)
	assert.NotEmpty(t, result.AgentCPUPct, "runner health still describes the runner")
}

func TestPollReadsEverythingOnAHealthyConnection(t *testing.T) {
	querier := &fakePollQuerier{row: fakeRow{values: pollRowValues()}}

	var result PollResult
	result.collect(context.Background(), querier, PollRequest{Target: testTarget()})

	assert.Empty(t, result.HeartbeatError)
	assert.NotEmpty(t, result.QueryMS)
	assert.Equal(t, "141", result.CurrentConnections)

	// The fixture row's backend PID belongs to no process here, so the check
	// answers for the runner it is actually on rather than trusting the row.
	assert.NotEqual(t, OnDBHostYes, result.AgentOnDBHost)
	assert.NotEmpty(t, result.AgentOnDBHostReason)
	assert.NotEmpty(t, result.DiskReason)
}

func TestPollDiskGate(t *testing.T) {
	for _, tt := range []struct {
		name          string
		verdict       string
		dataDirectory string
		want          string
	}{
		{"the database is on another machine", OnDBHostNo, "/var/lib/postgresql/17/main",
			diskReasonNotCoResident},
		{"the check could not decide", OnDBHostUnknown, "/var/lib/postgresql/17/main",
			diskReasonDBHostUnknown},
		{"this role may not read data_directory", OnDBHostYes, "", diskReasonSettingUnavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result := PollResult{AgentOnDBHost: tt.verdict}

			// nil querier: every branch here answers before any statement is sent.
			result.readDisk(context.Background(), nil, tt.dataDirectory, "")

			assert.Equal(t, tt.want, result.DiskReason)
			assert.Empty(t, result.DiskFreeBytes, "the runner's own disk is never sent instead")
			assert.Empty(t, result.DiskMount)
		})
	}
}

func TestPollDiskReadsTheDatabaseVolumes(t *testing.T) {
	dataDirectory := t.TempDir()

	result := PollResult{AgentOnDBHost: OnDBHostYes}
	result.readDisk(context.Background(), &fakePollQuerier{}, dataDirectory, "")

	assert.Empty(t, result.DiskReason)
	assert.NotEmpty(t, result.DiskFreeBytes)
	assert.NotEmpty(t, result.DiskTotalBytes)

	// pg_wal is listed beside the data directory, and on a cluster with no separate
	// WAL volume both name the same filesystem - either is a true answer.
	assert.Contains(t, []string{dataDirectory, dataDirectory + "/pg_wal"}, result.DiskMount)
}

func TestDedupePaths(t *testing.T) {
	assert.Equal(t, []string{"/data", "/data/pg_wal"},
		dedupePaths([]string{"/data", "/data/pg_wal", "/data", ""}))

	assert.Empty(t, dedupePaths([]string{"", ""}))
}

func TestFullestVolumeSkipsWhatItCannotRead(t *testing.T) {
	var result PollResult

	_, ok := result.fullestVolume([]string{"/nonexistent-volume-for-this-test"})

	assert.False(t, ok, "no readable path means read_failed, never the runner's own disk")
	assert.NotEmpty(t, result.Notes, "the skipped path is logged")
}

func TestRunnerHealthOnlyWhereTheRunnerIsNotTheDatabaseHost(t *testing.T) {
	confirmed := PollResult{AgentOnDBHost: OnDBHostYes}
	confirmed.readRunnerHealth()

	assert.Empty(t, confirmed.AgentCPUPct, "the top capture covers the runner on a confirmed host")
	assert.Empty(t, confirmed.RunnerLoad1)

	elsewhere := PollResult{AgentOnDBHost: OnDBHostNo}
	elsewhere.readRunnerHealth()

	assert.NotEmpty(t, elsewhere.AgentCPUPct)
}

// pollRowValues is one healthy PG17 row, in pollSQL's selection order.
func pollRowValues() []any {
	return []any{
		ptr(int32(48211)),
		ptr("ycrash_monitor"),
		ptr("orders"),
		ptr("127.0.0.1"),
		ptr(int32(52344)),
		&testPostmasterStart,
		ptr(int64(148)),
		ptr(int64(141)),
		ptr(int64(0)),
		ptr("200"),
		ptr("3"),
		ptr("0"),
		ptr("170006"),
		ptr("/var/lib/postgresql/17/main"),
		ptr("on"),
	}
}

func pollRowFrom(t *testing.T, values []any) pollRow {
	t.Helper()

	row, _, err := readPollRow(context.Background(), &fakePollQuerier{row: fakeRow{values: values}})
	require.NoError(t, err)

	return row
}

type fakePollQuerier struct {
	row      fakeRow
	deadline time.Time
}

func (f *fakePollQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.deadline, _ = ctx.Deadline()

	if sql != pollSQL {
		return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
	}

	return f.row
}

func (f *fakePollQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if sql != tablespaceSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	return &fakeRows{}, nil
}
