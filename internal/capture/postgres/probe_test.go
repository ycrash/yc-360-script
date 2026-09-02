package postgres

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	colInetClientAddr
	colInetClientPort
	colStatsReset
	colSettingNames
	colSettingValues
	colHasCheckpointer
	colHasGenericPlan
	colHasSessionFatal
	colPgStatStatements
	colHasPgMonitorRole
	colHasPgReadAllStats
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

func fullSettings() map[string]string {
	return map[string]string{
		"max_connections":            "200",
		"logging_collector":          "on",
		"update_process_title":       "on",
		"log_destination":            "csvlog",
		"log_directory":              "log",
		"log_filename":               "postgresql-%Y-%m-%d_%H%M%S.log",
		"log_line_prefix":            "%m [%p] ",
		"log_min_duration_statement": "500",

		"log_rotation_age":          "1440",
		"log_rotation_size":         "10240",
		"log_timezone":              "Etc/UTC",
		"log_min_messages":          "warning",
		"log_error_verbosity":       "default",
		"log_min_error_statement":   "error",
		"log_file_mode":             "0600",
		"log_parameter_max_length":  "1024",
		"track_activity_query_size": "1024",
		"shared_preload_libraries":  "pg_stat_statements,auto_explain",
		"compute_query_id":          "auto",
		"data_directory":            "/var/lib/postgresql/15/main",

		"track_io_timing":                   "off",
		"pg_stat_statements.max":            "5000",
		"pg_stat_statements.track":          "top",
		"pg_stat_statements.track_planning": "off",
		"pg_stat_statements.track_utility":  "on",

		"auto_explain.log_min_duration": "-1",
		"auto_explain.log_verbose":      "off",
		"auto_explain.log_analyze":      "off",
		"auto_explain.log_format":       "text",
		"auto_explain.sample_rate":      "1",
	}
}

var pgStatStatementsSettings = []string{
	"pg_stat_statements.max",
	"pg_stat_statements.track",
	"pg_stat_statements.track_planning",
	"pg_stat_statements.track_utility",
}

// autoExplainSettings share pg_stat_statements' shape: present only while the module is
// loaded in this session, so absence is "not loaded" rather than "denied".
var autoExplainSettings = []string{
	"auto_explain.log_min_duration",
	"auto_explain.log_verbose",
	"auto_explain.log_analyze",
	"auto_explain.log_format",
	"auto_explain.sample_rate",
}

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
	v[colInetClientAddr] = ptr("10.0.4.9")
	v[colInetClientPort] = ptr(int32(35484))
	v[colStatsReset] = ptr(testStatsReset)
	v[colSettingNames] = names
	v[colSettingValues] = values
	v[colHasCheckpointer] = ptr(false)
	v[colHasGenericPlan] = ptr(false)
	v[colHasSessionFatal] = ptr(true)
	v[colPgStatStatements] = ptr("1.10")
	v[colHasPgMonitorRole] = ptr(true)
	v[colHasPgReadAllStats] = ptr(true)
	v[colServerNow] = ptr(testServerNow)
	v[colServerClock] = ptr(testServerClock)

	return v
}

func logLocationValues(logfile any) []any {
	return []any{logfile}
}

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

func assign(dest, value any) error {
	d := reflect.ValueOf(dest)
	if d.Kind() != reflect.Pointer || d.IsNil() {
		return fmt.Errorf("destination %T is not a non-nil pointer", dest)
	}

	target := d.Elem()

	if value == nil {
		switch target.Kind() {
		case reflect.Pointer, reflect.Slice, reflect.Map, reflect.Interface:
			target.Set(reflect.Zero(target.Type()))
			return nil
		}

		return fmt.Errorf("cannot scan NULL into %s", target.Type())
	}

	v := reflect.ValueOf(value)
	if !v.Type().AssignableTo(target.Type()) {
		return fmt.Errorf("cannot assign %s to %s", v.Type(), target.Type())
	}

	target.Set(v)

	return nil
}

type fakeQuerier struct {
	serverFacts, logLocation, replication fakeRow

	// tablespaces answers the one row-set statement Collect sends. Its
	// bookkeeping is separate: the three single-row statements are asserted by
	// count and order, and this read is not one of them.
	tablespaces fakeResult

	sql       []string
	args      [][]any
	deadlines []time.Time

	rowSQL       []string
	rowDeadlines []time.Time
}

func (f *fakeQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.rowSQL = append(f.rowSQL, sql)

	deadline, _ := ctx.Deadline()
	f.rowDeadlines = append(f.rowDeadlines, deadline)

	if sql != tablespaceSQL {
		return nil, fmt.Errorf("unexpected query: %s", sql)
	}

	if f.tablespaces.err != nil {
		return nil, f.tablespaces.err
	}

	return &fakeRows{values: f.tablespaces.rows}, nil
}

func (f *fakeQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	f.sql = append(f.sql, sql)
	f.args = append(f.args, args)

	deadline, _ := ctx.Deadline()
	f.deadlines = append(f.deadlines, deadline)

	switch sql {
	case serverFactsSQL:
		return f.serverFacts
	case logLocationSQL, logLocationFormatSQL:
		return f.logLocation
	case replicationSQL:
		return f.replication
	}

	return fakeRow{err: fmt.Errorf("unexpected query: %s", sql)}
}

func healthyQuerier() *fakeQuerier {
	return &fakeQuerier{
		serverFacts: fakeRow{values: serverFactsValues()},
		logLocation: fakeRow{values: logLocationValues(ptr("log/postgresql-2026-08-04_000000.csv"))},
		replication: fakeRow{values: []any{ptr(int64(1))}},
		tablespaces: rowsResult([][]any{{"orders_archive", ptr("/srv/pg/archive")}}),
	}
}

func ptr[T any](v T) *T { return &v }

func collect(t *testing.T, q RowQuerier) Metadata {
	t.Helper()

	return Collect(context.Background(), q, testTarget(), testAgentNow)
}

func TestServerFactsColumnAlignment(t *testing.T) {
	var row serverFactsRow

	assert.Len(t, row.dest(), serverFactsColumnCount)
	assert.Len(t, serverFactsValues(), serverFactsColumnCount)
}

func TestServerFactsSendsTheSettingsCatalogue(t *testing.T) {
	q := healthyQuerier()
	collect(t, q)

	require.Equal(t,
		[]string{serverFactsSQL, logLocationFormatSQL, replicationSQL}, q.sql,
		"three statements, in order - the second names the format, because the no-argument "+
			"form's documented preference is stderr first, the inverse of the order that "+
			"serves the matcher. The same-host probe sends none of its own: every server-side "+
			"input it needs is already in the metadata by then")
	require.Len(t, q.args[0], 1)

	assert.Equal(t, []string{
		"max_connections",
		"logging_collector",
		"log_destination",
		"log_directory",
		"log_filename",
		"log_line_prefix",
		"log_rotation_age",
		"log_rotation_size",
		"log_timezone",
		"log_min_messages",
		"log_error_verbosity",
		"log_min_error_statement",
		"log_file_mode",
		"log_min_duration_statement",
		"log_parameter_max_length",
		"track_activity_query_size",
		"track_io_timing",
		"pg_stat_statements.max",
		"pg_stat_statements.track",
		"pg_stat_statements.track_planning",
		"pg_stat_statements.track_utility",
		"auto_explain.log_min_duration",
		"auto_explain.log_verbose",
		"auto_explain.log_analyze",
		"auto_explain.log_format",
		"auto_explain.sample_rate",
		"update_process_title",
		"shared_preload_libraries",
		"compute_query_id",
		"data_directory",
	}, q.args[0][0])

	assert.Equal(t, []any{"csvlog"}, q.args[1],
		"the format the cluster's log_destination declares, so a cluster writing both csvlog "+
			"and stderr does not hand back the harder of the two")
	assert.Empty(t, q.args[2], "replicationSQL takes no parameters")
}

func TestStatementDeadline(t *testing.T) {
	q := healthyQuerier()

	before := time.Now()
	collect(t, q)
	after := time.Now()

	require.Len(t, q.deadlines, 3, "every statement Collect sends, and it sends three")

	for i, deadline := range q.deadlines {
		assert.False(t, deadline.IsZero(), "statement %d ran with no deadline", i+1)
		assert.WithinRange(t, deadline, before.Add(StatementTimeout), after.Add(StatementTimeout),
			"statement %d", i+1)
	}
}

func TestCapabilityProbes(t *testing.T) {
	tests := []struct {
		name   string
		column int
		value  any
		want   func(*testing.T, Metadata)
	}{
		{

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

	assert.Equal(t, "500", m.LogMinDurationStatement)

	assert.Equal(t, "off", m.TrackIOTiming)
	assert.Equal(t, "5000", m.PgStatStatementsMax)
	assert.Equal(t, "top", m.PgStatStatementsTrack)
	assert.Equal(t, "off", m.PgStatStatementsTrackPlanning)
	assert.Equal(t, "on", m.PgStatStatementsTrackUtility)
}

func TestSettingsLibraryNotLoaded(t *testing.T) {
	visible := fullSettings()
	for _, name := range pgStatStatementsSettings {
		delete(visible, name)
	}

	visible["shared_preload_libraries"] = "auto_explain"

	q := healthyQuerier()
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(visible)

	m := collect(t, q)

	assert.Equal(t, "pg_stat_statements.max,pg_stat_statements.track,"+
		"pg_stat_statements.track_planning,pg_stat_statements.track_utility", m.SettingsUnavailable)

	assert.Empty(t, m.PgStatStatementsMax)
	assert.Equal(t, "off", m.TrackIOTiming, "a core GUC, which does not depend on the library")
	assert.Empty(t, m.QueryError, "an unloaded library returns no row rather than erroring")
}

func TestSettingsAutoExplainNotLoaded(t *testing.T) {
	visible := fullSettings()
	for _, name := range autoExplainSettings {
		delete(visible, name)
	}

	visible["shared_preload_libraries"] = "pg_stat_statements"

	q := healthyQuerier()
	q.serverFacts.values[colSettingNames], q.serverFacts.values[colSettingValues] = settingsColumns(visible)

	m := collect(t, q)

	assert.Equal(t, "auto_explain.log_min_duration,auto_explain.log_verbose,"+
		"auto_explain.log_analyze,auto_explain.log_format,auto_explain.sample_rate",
		m.SettingsUnavailable)

	assert.Empty(t, m.AutoExplainLogMinDuration)
	assert.Empty(t, m.AutoExplainLogVerbose)
	assert.Empty(t, m.QueryError, "an unloaded module returns no row rather than erroring")

	assert.Equal(t, "5000", m.PgStatStatementsMax,
		"the other module's GUCs are unaffected: one absence does not imply the other")
}

func TestSettingsAutoExplainLoaded(t *testing.T) {
	m := collect(t, healthyQuerier())

	assert.Equal(t, "-1", m.AutoExplainLogMinDuration,
		"the default: loaded and logging nothing, which is why plans_harvested=0 is not a failure")
	assert.Equal(t, "off", m.AutoExplainLogVerbose, "so logged plans carry no join key")
	assert.Equal(t, "off", m.AutoExplainLogAnalyze, "so they carry estimates, not timings")
	assert.Equal(t, "text", m.AutoExplainLogFormat, "so the identifier line is parseable")
	assert.Equal(t, "1", m.AutoExplainSampleRate, "so an absent plan does not mean sampling")
}

func TestServerFactsCarryTheGenericPlanCapability(t *testing.T) {
	for _, present := range []bool{true, false} {
		q := healthyQuerier()
		q.serverFacts.values[colHasGenericPlan] = ptr(present)

		assert.Equal(t, strconv.FormatBool(present), collect(t, q).HasGenericPlan,
			"recorded as a semantic flag, so nobody re-derives it from server_version_num")
	}
}

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

	assert.Equal(t, "log_directory,log_filename,shared_preload_libraries,data_directory", m.SettingsUnavailable)

	assert.Equal(t, "200", m.MaxConnections)
	assert.Equal(t, "auto", m.ComputeQueryID)
	assert.Equal(t, "150004", m.ServerVersionNum)
	assert.Empty(t, m.QueryError)
}

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
		"log_line_prefix,log_rotation_age,log_rotation_size,log_timezone,log_min_messages,"+
		"log_error_verbosity,log_min_error_statement,log_file_mode,"+
		"log_min_duration_statement,log_parameter_max_length,"+
		"track_activity_query_size,track_io_timing,pg_stat_statements.max,"+
		"pg_stat_statements.track,pg_stat_statements.track_planning,"+
		"pg_stat_statements.track_utility,auto_explain.log_min_duration,"+
		"auto_explain.log_verbose,auto_explain.log_analyze,auto_explain.log_format,"+
		"auto_explain.sample_rate,update_process_title,shared_preload_libraries,"+
		"compute_query_id,data_directory", m.SettingsUnavailable)
	assert.Empty(t, m.QueryError)
	assert.Equal(t, "orders_db", m.CurrentDatabase)
}

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

	jakarta := time.FixedZone("WIB", 7*60*60)
	q.serverFacts.values[colServerNow] = ptr(testServerNow.In(jakarta))

	m := collect(t, q)

	assert.Equal(t, "2026-08-04T09:12:44.102Z", m.ServerNow)
	assert.Equal(t, "2026-08-04T09:12:44.104Z", m.ServerClockTimestamp)
	assert.Equal(t, "2026-07-30T02:11:09.482Z", m.PostmasterStartTime)
	assert.Equal(t, "2026-07-01T04:00:00.000Z", m.StatsReset)

	assert.True(t, m.AgentTSAtClockRead.After(testAgentNow),
		"the agent's clock is read beside the server's, not when the collector was built - "+
			"the gap between the two is connect plus every earlier statement, and it would "+
			"otherwise land in the skew figure this row exists to give")
}

func TestErrorText(t *testing.T) {
	t.Run("nil is empty", func(t *testing.T) {
		assert.Empty(t, errorText(nil, testPassword))
	})

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

	t.Run("empty password is not substituted", func(t *testing.T) {
		assert.Equal(t, "connection refused", errorText(errors.New("connection refused"), ""))
	})
}
