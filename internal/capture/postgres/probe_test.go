package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run against fakes: what they pin is decided in Go and is the same
// on every server version. What needs a real server and a real least-privilege
// role lives in integration_test.go, behind the pgintegration tag.

// serverFactsSQL's columns, in selection order. Named so a reordered SELECT
// list fails TestServerFactsColumnAlignment instead of silently mapping values
// onto the wrong fields.
const (
	colCurrentDatabase = iota
	colCurrentUser
	colVersion
	colServerVersionNum
	colIsInRecovery
	colPostmasterStart
	colBackendPID
	colInetServerAddr
	colInetServerPort
	colStatsReset
	colSettingNames
	colSettingValues
	colHasCheckpointer
	colHasSessionFatal
	colPgStatStatements
	colHasPgMonitorRole
	colServerNow
	colServerClock

	serverFactsColumnCount
)

var (
	testPostmasterStart = time.Date(2026, 7, 30, 2, 11, 9, 482_000_000, time.UTC)
	testStatsReset      = time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC)
	testServerNow       = time.Date(2026, 8, 4, 9, 12, 44, 102_000_000, time.UTC)
	testServerClock     = time.Date(2026, 8, 4, 9, 12, 44, 104_000_000, time.UTC)
	testAgentNow        = time.Date(2026, 8, 4, 9, 12, 44, 118_000_000, time.UTC)
)

// What a role that can see everything gets back.
func fullSettings() map[string]string {
	return map[string]string{
		"max_connections":            "200",
		"logging_collector":          "on",
		"log_destination":            "csvlog",
		"log_directory":              "log",
		"log_filename":               "postgresql-%Y-%m-%d_%H%M%S.log",
		"log_line_prefix":            "%m [%p] ",
		"log_min_duration_statement": "500",
		"log_parameter_max_length":   "1024",
		"shared_preload_libraries":   "pg_stat_statements,auto_explain",
		"compute_query_id":           "auto",
		"data_directory":             "/var/lib/postgresql/15/main",
	}
}

// Renders a name/value map the way serverFactsSQL's two array_agg subqueries
// do: parallel arrays ordered by name.
func settingsColumns(settings map[string]string) (names []string, values []string) {
	for _, s := range capturedSettings {
		value, ok := settings[s.name]
		if !ok {
			continue
		}

		names = append(names, s.name)
		values = append(values, value)
	}

	return names, values
}

// A complete serverFactsSQL result. Tests take a copy and overwrite the one
// column they are about.
func serverFactsValues() []any {
	names, values := settingsColumns(fullSettings())

	v := make([]any, serverFactsColumnCount)
	v[colCurrentDatabase] = ptr("orders_db")
	v[colCurrentUser] = ptr("ycrash_monitor")
	v[colVersion] = ptr("PostgreSQL 15.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0")
	v[colServerVersionNum] = ptr("150004")
	v[colIsInRecovery] = ptr(false)
	v[colPostmasterStart] = ptr(testPostmasterStart)
	v[colBackendPID] = ptr(int32(48211))
	v[colInetServerAddr] = ptr("10.0.4.7")
	v[colInetServerPort] = ptr(int32(5432))
	v[colStatsReset] = ptr(testStatsReset)
	v[colSettingNames] = names
	v[colSettingValues] = values
	v[colHasCheckpointer] = ptr(false)
	v[colHasSessionFatal] = ptr(true)
	v[colPgStatStatements] = ptr("1.10")
	v[colHasPgMonitorRole] = ptr(true)
	v[colServerNow] = ptr(testServerNow)
	v[colServerClock] = ptr(testServerClock)

	return v
}

// nil is logging_collector off. data_directory is deliberately not here - it
// rides in the settings catalogue.
func logLocationValues(logfile any) []any {
	return []any{logfile}
}

// Answers a single statement with values in selection order, or with an error.
type fakeRow struct {
	values []any
	err    error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}

	if len(dest) != len(r.values) {
		return fmt.Errorf("scan into %d destinations from %d values", len(dest), len(r.values))
	}

	for i := range dest {
		if err := assign(dest[i], r.values[i]); err != nil {
			return fmt.Errorf("column %d: %w", i, err)
		}
	}

	return nil
}

// Types have to match exactly - a wrong type fails the scan rather than being
// coerced - so a change to serverFactsRow's field types shows up here.
func assign(dest, value any) error {
	d := reflect.ValueOf(dest)
	if d.Kind() != reflect.Pointer || d.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", dest)
	}

	target := d.Elem()

	if value == nil {
		target.Set(reflect.Zero(target.Type()))
		return nil
	}

	v := reflect.ValueOf(value)
	if !v.Type().AssignableTo(target.Type()) {
		return fmt.Errorf("cannot assign %s to %s", v.Type(), target.Type())
	}

	target.Set(v)

	return nil
}

// Routes by statement text, so a test that changes one of the three SQL
// constants without updating its fake gets an explicit "unexpected query"
// rather than a silently empty result.
type fakeQuerier struct {
	serverFacts, logLocation, replication fakeRow

	// Recorded for the tests that assert what was sent and under what deadline.
	sql       []string
	args      [][]any
	deadlines []time.Time
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args)

	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)

	switch sql {
	case serverFactsSQL:
		return f.serverFacts
	case logLocationSQL:
		return f.logLocation
	case replicationSQL:
		return f.replication
	}

	return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

// Answers all three statements from a fully readable database.
func healthyQuerier() *fakeQuerier {
	return &fakeQuerier{
		serverFacts: fakeRow{values: serverFactsValues()},
		logLocation: fakeRow{values: logLocationValues(ptr("log/postgresql-2026-08-04_000000.csv"))},
		replication: fakeRow{values: []any{ptr(int64(1))}},
	}
}

func ptr[T any](v T) *T { return &v }

func collect(t *testing.T, q Querier) Metadata {
	t.Helper()

	return Collect(context.Background(), q, testTarget(), testAgentNow)
}

// The column constants, the scan destinations and the fake's value slice all
// describe serverFactsSQL's SELECT list; nothing else notices when one moves.
func TestServerFactsColumnAlignment(t *testing.T) {
	var row serverFactsRow

	assert.Len(t, row.dest(), serverFactsColumnCount)
	assert.Len(t, serverFactsValues(), serverFactsColumnCount)
}

func TestServerFactsSendsTheSettingsCatalogue(t *testing.T) {
	q := healthyQuerier()
	collect(t, q)

	require.Equal(t, []string{serverFactsSQL, logLocationSQL, replicationSQL}, q.sql, "three statements, in order")
	require.Len(t, q.args[0], 1)

	assert.Equal(t, []string{
		"max_connections",
		"logging_collector",
		"log_destination",
		"log_directory",
		"log_filename",
		"log_line_prefix",
		"log_min_duration_statement",
		"log_parameter_max_length",
		"shared_preload_libraries",
		"compute_query_id",
		"data_directory",
	}, q.args[0][0])

	assert.Empty(t, q.args[1], "logLocationSQL takes no parameters")
	assert.Empty(t, q.args[2], "replicationSQL takes no parameters")
}

func TestStatementDeadline(t *testing.T) {
	q := healthyQuerier()

	before := time.Now()
	collect(t, q)
	after := time.Now()

	require.Len(t, q.deadlines, 3)

	for i, deadline := range q.deadlines {
		assert.False(t, deadline.IsZero(), "statement %d ran with no deadline", i+1)
		assert.WithinRange(t, deadline, before.Add(StatementTimeout), after.Add(StatementTimeout),
			"statement %d", i+1)
	}
}

// Each probe is semantic rather than version arithmetic, so these vary the
// catalog's answer and never the version number.
func TestCapabilityProbes(t *testing.T) {
	tests := []struct {
		name   string
		column int
		value  any
		want   func(*testing.T, Metadata)
	}{
		{
			// The case version arithmetic gets wrong: pg_upgrade carries the old
			// extension schema forward until someone runs ALTER EXTENSION ...
			// UPDATE, so a PG15 server can legitimately expose the pre-1.8
			// column set.
			name:   "pg_stat_statements at 1.10",
			column: colPgStatStatements,
			value:  ptr("1.10"),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "true", m.HasPgStatStatements)
				assert.Equal(t, "1.10", m.PgStatStatementsVersion)
			},
		},
		{
			name:   "pg_stat_statements at 1.7",
			column: colPgStatStatements,
			value:  ptr("1.7"),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "true", m.HasPgStatStatements)
				assert.Equal(t, "1.7", m.PgStatStatementsVersion)
			},
		},
		{
			// The extension not being installed is an empty extversion, not a
			// NULL: serverFactsSQL coalesces so the flag is decided by one
			// rule.
			name:   "pg_stat_statements absent",
			column: colPgStatStatements,
			value:  ptr(""),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "false", m.HasPgStatStatements)
				assert.Empty(t, m.PgStatStatementsVersion)
			},
		},
		{
			name:   "pg_stat_checkpointer present, as on 17 and 18",
			column: colHasCheckpointer,
			value:  ptr(true),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "true", m.HasPgStatCheckpointer)
			},
		},
		{
			name:   "pg_stat_checkpointer absent, as on 14 through 16",
			column: colHasCheckpointer,
			value:  ptr(false),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "false", m.HasPgStatCheckpointer)
			},
		},
		{
			name:   "sessions_fatal present",
			column: colHasSessionFatal,
			value:  ptr(true),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "true", m.HasSessionFatalStats)
			},
		},
		{
			// Synthetic across the declared 14-18 range: sessions_fatal landed
			// in PG14, so no supported server answers false. The probe is
			// out-of-range defence for a PG13 cluster somebody points at this.
			name:   "sessions_fatal absent",
			column: colHasSessionFatal,
			value:  ptr(false),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "false", m.HasSessionFatalStats)
			},
		},
		{
			name:   "pg_monitor granted",
			column: colHasPgMonitorRole,
			value:  ptr(true),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "true", m.HasPgMonitorRole)
			},
		},
		{
			name:   "pg_monitor not granted",
			column: colHasPgMonitorRole,
			value:  ptr(false),
			want: func(t *testing.T, m Metadata) {
				assert.Equal(t, "false", m.HasPgMonitorRole)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := healthyQuerier()
			q.serverFacts.values[tt.column] = tt.value

			tt.want(t, collect(t, q))
		})
	}
}

func TestSettingsFullVisibility(t *testing.T) {
	m := collect(t, healthyQuerier())

	assert.Equal(t, "200", m.MaxConnections)
	assert.Equal(t, "on", m.LoggingCollector)
	assert.Equal(t, "csvlog", m.LogDestination)
	assert.Equal(t, "log", m.LogDirectory)
	assert.Equal(t, "postgresql-%Y-%m-%d_%H%M%S.log", m.LogFilename)
	assert.Equal(t, "%m [%p] ", m.LogLinePrefix)
	assert.Equal(t, "1024", m.LogParameterMaxLength)
	assert.Equal(t, "pg_stat_statements,auto_explain", m.SharedPreloadLibraries)
	assert.Equal(t, "auto", m.ComputeQueryID)
	assert.Equal(t, "/var/lib/postgresql/15/main", m.DataDirectory)
	assert.Empty(t, m.SettingsUnavailable)

	// Internal units, as pg_settings.setting renders them - not SHOW's `500ms`.
	assert.Equal(t, "500", m.LogMinDurationStatement)
}

// The four settings dropped here are GUC_SUPERUSER_ONLY, so pg_settings omits
// their rows for a role without pg_read_all_settings.
func TestSettingsRestrictedRole(t *testing.T) {
	visible := fullSettings()
	delete(visible, "log_directory")
	delete(visible, "log_filename")
	delete(visible, "shared_preload_libraries")
	delete(visible, "data_directory")

	q := healthyQuerier()
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(visible)
	q.serverFacts.values[colHasPgMonitorRole] = ptr(false)

	m := collect(t, q)

	assert.Empty(t, m.LogDirectory)
	assert.Empty(t, m.LogFilename)
	assert.Empty(t, m.SharedPreloadLibraries)
	assert.Empty(t, m.DataDirectory)

	// Named in catalogue order, so the value is stable across runs. This is what
	// disambiguates shared_preload_libraries "" meaning none configured from ""
	// meaning not visible to this role.
	assert.Equal(t, "log_directory,log_filename,shared_preload_libraries,data_directory", m.SettingsUnavailable)

	// Everything else survives: nothing about serverFactsSQL is all-or-nothing.
	assert.Equal(t, "200", m.MaxConnections)
	assert.Equal(t, "auto", m.ComputeQueryID)
	assert.Equal(t, "150004", m.ServerVersionNum)
	assert.Empty(t, m.QueryError)
}

// The other reason a row can be missing. The recorded fact is deliberately
// identical to the privilege case; the file carries what tells them apart.
func TestSettingsUnknownToThisVersion(t *testing.T) {
	visible := fullSettings()
	delete(visible, "compute_query_id")

	q := healthyQuerier()
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(visible)

	m := collect(t, q)

	assert.Empty(t, m.ComputeQueryID)
	assert.Equal(t, "compute_query_id", m.SettingsUnavailable)
	assert.Empty(t, m.QueryError, "an unknown name returns no row rather than erroring")
}

func TestSettingsNoneVisible(t *testing.T) {
	q := healthyQuerier()
	q.serverFacts.values[colSettingNames] = nil
	q.serverFacts.values[colSettingValues] = nil

	m := collect(t, q)

	assert.Equal(t, "max_connections,logging_collector,log_destination,log_directory,log_filename,"+
		"log_line_prefix,log_min_duration_statement,log_parameter_max_length,shared_preload_libraries,"+
		"compute_query_id,data_directory", m.SettingsUnavailable)
	assert.Empty(t, m.QueryError)
	assert.Equal(t, "orders_db", m.CurrentDatabase)
}

// A Unix socket connection has no server address or port, and a database whose
// statistics have never been reset has no stats_reset.
func TestNullServerFacts(t *testing.T) {
	q := healthyQuerier()
	q.serverFacts.values[colInetServerAddr] = nil
	q.serverFacts.values[colInetServerPort] = nil
	q.serverFacts.values[colStatsReset] = nil

	m := collect(t, q)

	assert.Empty(t, m.InetServerAddr)
	assert.Empty(t, m.InetServerPort)
	assert.Empty(t, m.StatsReset)
	assert.Empty(t, m.QueryError, "a NULL is a value, not a failure")
}

func TestTimestampsAreRecordedRaw(t *testing.T) {
	q := healthyQuerier()

	// A server in a non-UTC zone renders the same instant.
	jakarta := time.FixedZone("WIB", 7*60*60)
	q.serverFacts.values[colServerNow] = ptr(testServerNow.In(jakarta))

	m := collect(t, q)

	assert.Equal(t, "2026-08-04T09:12:44.102Z", m.ServerNow)
	assert.Equal(t, "2026-08-04T09:12:44.104Z", m.ServerClockTimestamp)
	assert.Equal(t, "2026-07-30T02:11:09.482Z", m.PostmasterStartTime)
	assert.Equal(t, "2026-07-01T04:00:00.000Z", m.StatsReset)
	assert.Equal(t, testAgentNow, m.AgentTSAtClockRead)
}

func TestErrorText(t *testing.T) {
	t.Run("nil is empty", func(t *testing.T) {
		assert.Empty(t, errorText(nil, testPassword))
	})

	// A driver message with a newline in it would put a record boundary inside
	// a CSV field. This artifact's claim is that it needs no record-aware
	// parsing, and that claim must not depend on which error came back.
	t.Run("flattened onto one line", func(t *testing.T) {
		err := errors.New("ERROR: permission denied\nDETAIL: role is not a member\r\nHINT: grant it")

		got := errorText(err, "")

		assert.Equal(t, "ERROR: permission denied DETAIL: role is not a member HINT: grant it", got)
		assert.NotContains(t, got, "\n")
	})

	t.Run("password removed", func(t *testing.T) {
		err := fmt.Errorf("connection refused (password=%s)", testPassword)

		got := errorText(err, testPassword)

		assert.NotContains(t, got, testPassword)
		assert.Contains(t, got, "<redacted>")
	})

	// The guard that matters: an empty password must not turn every position in
	// the string into a redaction.
	t.Run("empty password is not substituted", func(t *testing.T) {
		assert.Equal(t, "connection refused", errorText(errors.New("connection refused"), ""))
	})
}
