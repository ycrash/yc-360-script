package postgres

import (
	"bytes"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testConnectFailureNow = time.Date(2026, 8, 4, 9, 12, 49, 201_000_000, time.UTC)

func fullArtifactMetadata() Metadata {
	return Metadata{
		AgentTS:        testAgentNow,
		YC360Version:   "3.6.1",
		Tablespaces:    []Tablespace{{Name: "orders_archive", Location: "/srv/pg/archive"}},
		TargetHost:     "db-prod-01.internal",
		TargetPort:     5432,
		TargetDatabase: "orders_db",
		TargetUsername: "ycrash_monitor",
		TargetSSLMode:  "require",

		ExplainMode:     ExplainModeAll,
		ExplainLiterals: explainLiteralsVerbatim,

		LogAccess: LogAccessDirect,

		CurrentDatabase:     "orders_db",
		CurrentUser:         "ycrash_monitor",
		BackendPID:          "48211",
		InetServerAddr:      "10.0.4.7",
		InetServerPort:      "5432",
		IsInRecovery:        "false",
		PostmasterStartTime: timestamp(testPostmasterStart),
		StatsReset:          timestamp(testStatsReset),
		Version:             "PostgreSQL 17.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0",
		ServerVersionNum:    "170004",

		MaxConnections:          "200",
		LoggingCollector:        "on",
		LogDestination:          "csvlog",
		LogDirectory:            "log",
		LogFilename:             "postgresql-%Y-%m-%d_%H%M%S.log",
		LogLinePrefix:           "%m [%p] ",
		LogRotationAge:          "1440",
		LogRotationSize:         "10240",
		LogTimezone:             "Etc/UTC",
		LogMinMessages:          "warning",
		LogErrorVerbosity:       "default",
		LogMinErrorStatement:    "error",
		LogFileMode:             "0600",
		LogMinDurationStatement: "500",
		LogCheckpoints:          "on",
		LogParameterMaxLength:   "1024",
		TrackActivityQuerySize:  "1024",

		TrackIOTiming:                 "off",
		PgStatStatementsMax:           "5000",
		PgStatStatementsTrack:         "top",
		PgStatStatementsTrackPlanning: "off",
		PgStatStatementsTrackUtility:  "on",

		AutoExplainLogMinDuration: "0",
		AutoExplainLogVerbose:     "on",
		AutoExplainLogAnalyze:     "off",
		AutoExplainLogFormat:      "text",
		AutoExplainSampleRate:     "1",

		UpdateProcessTitle:     "on",
		SharedPreloadLibraries: "pg_stat_statements,auto_explain",

		DataDirectory:          "/var/lib/postgresql/17/main",
		CurrentLogfile:         "log/postgresql-2026-08-04_000000.csv",
		CurrentLogfileResolved: "/var/lib/postgresql/17/main/log/postgresql-2026-08-04_000000.csv",
		LogResolvedBy:          resolvedByCurrentLogfiles,
		LogFormats:             "csvlog",

		AgentOnDBHost:         OnDBHostYes,
		AgentOnDBHostBy:       confirmedByBackendPID,
		AgentOnDBHostEvidence: evidenceLogFile + "," + evidenceServerAddrMatch,

		HasPgMonitorRole:        "true",
		HasPgReadAllStats:       "true",
		HasPgStatStatements:     "true",
		PgStatStatementsVersion: "1.11",
		HasPgStatCheckpointer:   "true",
		HasGenericPlan:          "true",
		HasSessionFatalStats:    "true",
		ComputeQueryID:          "auto",

		ReplicationConfigured: "true",

		ServerNow:            timestamp(testServerNow),
		ServerClockTimestamp: timestamp(testServerClock),
		AgentTSAtClockRead:   testAgentNow,
		ClockReadRTTMS:       "1.4",
		ConnectMS:            "12.4",
	}
}

func connectFailureMetadata() Metadata {
	return Metadata{
		AgentTS:            testConnectFailureNow,
		AgentTSAtClockRead: testConnectFailureNow,
		YC360Version:       "3.6.1",
		TargetHost:         "db-prod-01.internal",
		TargetPort:         5432,
		TargetDatabase:     "orders_db",
		TargetUsername:     "ycrash_monitor",
		TargetSSLMode:      "require",

		ExplainMode:     ExplainModeAll,
		ExplainLiterals: explainLiteralsVerbatim,

		LogAccess:    LogAccessUnknown,
		ConnectError: ErrTooManyConnections.Error(),
	}
}

func writeArtifact(t *testing.T, m Metadata) string {
	t.Helper()

	var buf bytes.Buffer

	target := SampleContext{At: m.AgentTS, Database: m.TargetDatabase}
	require.NoError(t, writeMetadataBlock(&buf, "pg_metadata_target", []headerField{
		{"db", target.Database},
		{"dbid", target.DBID},
	}, targetFields(m), target.At))

	server := []headerField{
		{"db", m.CurrentDatabase},
		{"dbid", "16401"},
		{"sample", "1"},
	}

	require.NoError(t, writeMetadataBlock(&buf, "pg_metadata_server", server, serverBlockFields(m), m.AgentTSAtClockRead))
	require.NoError(t, writeTablespaceBlock(&buf, server, m, m.AgentTSAtClockRead))

	return buf.String()
}

func golden(t *testing.T, name string) string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)

	return string(content)
}

// parseArtifact reads the key,value blocks into one map and one key list. The
// tablespace block is the file's one tabular body: it is checked for shape here
// and read by parseTablespaceBlock, since a tablespace's name is data, not a key.
func parseArtifact(t *testing.T, artifact string) (headers []string, values map[string]string, keys []string) {
	t.Helper()

	values = map[string]string{}

	var (
		body        strings.Builder
		tablespaces bool
	)

	flush := func() {
		if body.Len() == 0 {
			return
		}

		reader := csv.NewReader(strings.NewReader(body.String()))
		reader.FieldsPerRecord = -1

		records, err := reader.ReadAll()
		require.NoError(t, err)
		require.NotEmpty(t, records)

		if tablespaces {
			require.Equal(t, tablespaceLocationColumns, records[0],
				"the tablespace block opens with its own column header")

			for _, record := range records[1:] {
				require.Len(t, record, 2, "every tablespace record is one name and one location")
			}

			body.Reset()

			return
		}

		require.Equal(t, []string{"key", "value"}, records[0],
			"a block's body opens with its own column header")

		for _, record := range records[1:] {
			require.Len(t, record, 2, "every body record is one key and one value")

			values[record[0]] = record[1]
			keys = append(keys, record[0])
		}

		body.Reset()
	}

	for line := range strings.SplitSeq(strings.TrimSuffix(artifact, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			flush()
			headers = append(headers, line)
			tablespaces = strings.Contains(line, "source=pg_metadata_tablespaces ")

			continue
		}

		body.WriteString(line)
		body.WriteString("\n")
	}

	flush()

	return headers, values, keys
}

// parseTablespaceBlock returns the tablespace block's header line and its rows.
func parseTablespaceBlock(t *testing.T, artifact string) (header string, rows [][]string) {
	t.Helper()

	var (
		body   strings.Builder
		inside bool
	)

	for line := range strings.SplitSeq(strings.TrimSuffix(artifact, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			inside = strings.Contains(line, "source=pg_metadata_tablespaces ")
			if inside {
				header = line
			}

			continue
		}

		if inside {
			body.WriteString(line)
			body.WriteString("\n")
		}
	}

	require.NotEmpty(t, header, "the artifact has no tablespace block")

	records, err := csv.NewReader(strings.NewReader(body.String())).ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, records)
	require.Equal(t, tablespaceLocationColumns, records[0])

	return header, records[1:]
}

func TestEveryShippedGoldenDeclaresOneFormat(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("testdata", "*.txt"))
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	for _, path := range entries {
		name := filepath.Base(path)

		t.Run(name, func(t *testing.T) {
			want := "format=csv"
			for _, prefix := range []string{"pg_deadlocks_", "pg_timeouts_", "pg_checkpoint_log_", "pg_explain_"} {
				if strings.HasPrefix(name, prefix) {
					want = "format=text"
				}
			}

			headers := 0
			for line := range strings.SplitSeq(golden(t, name), "\n") {
				if !strings.HasPrefix(line, "# engine=postgres ") {
					continue
				}

				headers++
				assert.Contains(t, strings.Fields(line), want,
					"every block of one file declares that file's format, the header-only ones included")
			}

			assert.NotZero(t, headers, "a golden with no block header is not a golden")
		})
	}
}

func TestGoldenKeepsTrailingWhitespace(t *testing.T) {
	assert.Contains(t, golden(t, "pg_metadata_full.txt"), "log_line_prefix,%m [%p] \n",
		"the golden's log_line_prefix must keep its trailing space")
}

func TestBlockHeaderFieldOrder(t *testing.T) {
	headers, _, _ := parseArtifact(t, writeArtifact(t, fullArtifactMetadata()))
	require.Len(t, headers, 3, "the target block, the server block and the tablespace block")

	assert.Equal(t, []string{
		"#",
		"engine=postgres",
		"source=pg_metadata_target",
		"v=1",
		"format=csv",
		"scope=cluster",
		"db=orders_db",
		"dbid=",
		"ts=2026-08-04T09:12:44.118Z",
	}, strings.Fields(headers[0]))

	assert.Equal(t, []string{
		"#",
		"engine=postgres",
		"source=pg_metadata_server",
		"v=1",
		"format=csv",
		"scope=cluster",
		"db=orders_db",
		"dbid=16401",
		"sample=1",
		"ts=2026-08-04T09:12:44.118Z",
	}, strings.Fields(headers[1]))

	assert.Equal(t, []string{
		"#",
		"engine=postgres",
		"source=pg_metadata_tablespaces",
		"v=1",
		"format=csv",
		"scope=cluster",
		"db=orders_db",
		"dbid=16401",
		"sample=1",
		"ts=2026-08-04T09:12:44.118Z",
	}, strings.Fields(headers[2]), "the same sample as the server block, and no error= when the read worked")
}

func TestBlockHeaderIsNotACSVRecord(t *testing.T) {
	refusal := "failed to connect to `user=ycrash_monitor database=orders_db`: " +
		"hostname resolving error: lookup db-prod-01.internal: no such host"

	var buf bytes.Buffer
	require.NoError(t, writeBlockHeader(&buf, "pg_metadata", "cluster", []headerField{
		{"status", StatusConnectFailed},
		{"connect_error", refusal},
	}, testAgentNow))

	header := buf.String()

	assert.Contains(t, header, `connect_error="`+refusal+`"`,
		"headerValue quotes any value carrying whitespace, which is every driver message")

	reader := csv.NewReader(strings.NewReader(header))
	reader.FieldsPerRecord = -1

	_, err := reader.ReadAll()
	require.Error(t, err,
		"the header is a bare quote in a non-quoted field. If this ever parses cleanly, "+
			"the reader requirements can be simplified - until then they cannot")

	headers, _, _ := parseArtifact(t, header+"key,value\ncapture_mode,unknown\n")
	require.Len(t, headers, 1)
	assert.Equal(t, strings.TrimSuffix(header, "\n"), headers[0])
}

func TestServerBlockCarriesNoConnectError(t *testing.T) {
	_, values, keys := parseArtifact(t, writeArtifact(t, fullArtifactMetadata()))

	assert.NotContains(t, values, "connect_error",
		"the key exists only where it can be non-empty, which is the closing block's header")

	assert.Equal(t, "log_access", keys[len(targetFields(fullArtifactMetadata()))],
		"the server block opens with the log-access fact")
}

func TestQueryErrorStillWritesEveryKey(t *testing.T) {
	m := connectFailureMetadata()
	m.AgentTS = testAgentNow
	m.AgentTSAtClockRead = testAgentNow
	m.LogAccess = LogAccessNone
	m.ConnectError = ""
	m.QueryError = "ERROR: canceling statement due to statement timeout (SQLSTATE 57014)"

	_, values, keys := parseArtifact(t, writeArtifact(t, m))

	_, _, want := parseArtifact(t, golden(t, "pg_metadata_full.txt"))
	assert.Equal(t, want, keys, "the key set does not depend on what could be read")

	assert.Equal(t, m.QueryError, values["query_error"])
	for _, key := range []string{
		"current_database",
		"server_version_num",
		"max_connections",
		"has_pg_stat_checkpointer",
		"server_now",
	} {
		assert.Empty(t, values[key], "%s should be written empty, not filled", key)
	}
}

func TestAwkwardValuesRoundTrip(t *testing.T) {
	m := fullArtifactMetadata()
	m.LogLinePrefix = `%m [%p] app=%a,user=%u "session"`
	m.Version = "PostgreSQL 17.4 on x86_64-pc-linux-gnu, compiled by gcc 12.2.0, 64-bit"
	m.SharedPreloadLibraries = "pg_stat_statements,auto_explain,pg_cron"

	_, values, _ := parseArtifact(t, writeArtifact(t, m))

	assert.Equal(t, m.LogLinePrefix, values["log_line_prefix"])
	assert.Equal(t, m.Version, values["version"])
	assert.Equal(t, m.SharedPreloadLibraries, values["shared_preload_libraries"])
}

func TestValuesAreFlattened(t *testing.T) {
	m := fullArtifactMetadata()
	m.LogLinePrefix = "%m [%p]\n> "
	m.QueryError = "ERROR: permission denied\r\nDETAIL: role is not a member"

	artifact := writeArtifact(t, m)
	_, values, keys := parseArtifact(t, artifact)

	assert.Equal(t, "%m [%p] > ", values["log_line_prefix"])
	assert.Equal(t, "ERROR: permission denied DETAIL: role is not a member", values["query_error"])

	lines := strings.Split(strings.TrimSuffix(artifact, "\n"), "\n")
	assert.Len(t, lines, len(keys)+4+2+len(m.Tablespaces),
		"one row is one line, plus two key,value blocks' block and column headers, plus the "+
			"tablespace block's two headers and its rows")
}

func TestTargetFieldsAreWhatWasConfigured(t *testing.T) {
	m := fullArtifactMetadata()

	keys := make([]string, 0, len(targetFields(m)))
	for _, f := range targetFields(m) {
		keys = append(keys, f.key)
	}

	assert.Equal(t, []string{
		"agent_ts",
		"yc360_version",
		"target_host",
		"target_port",
		"target_database",
		"target_username",
		"target_sslmode",
		"explain_mode",
		"explain_literals",
	}, keys)

	_, values, _ := parseArtifact(t, writeArtifact(t, m))
	assert.Equal(t, "db-prod-01.internal", values["target_host"])

	assert.Equal(t, ExplainModeAll, values["explain_mode"],
		"the run's intent, in the block written before dialling")
	assert.Equal(t, "verbatim", values["explain_literals"])
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteErrorsPropagate(t *testing.T) {
	sinkErr := errors.New("no space left on device")
	sink := failingWriter{err: sinkErr}

	m := fullArtifactMetadata()

	collector := NewMetadata(testTarget(), "3.6.1", testAgentNow, "")
	assert.ErrorIs(t, collector.WriteOpening(sink, SampleContext{At: testAgentNow}), sinkErr)

	assert.ErrorIs(t,
		writeMetadataBlock(sink, "pg_metadata_server", nil, serverBlockFields(m), testAgentNow),
		sinkErr)

	m.QueryError = strings.Repeat("x", 1<<14)
	assert.ErrorIs(t,
		writeMetadataBlock(sink, "pg_metadata_server", nil, serverBlockFields(m), testAgentNow),
		sinkErr)
}

// blockHeader renders at the database scope, the one every caller below asserts on.
func blockHeader(t *testing.T, source string, fields []headerField) string {
	t.Helper()

	var buf bytes.Buffer
	require.NoError(t, writeBlockHeader(&buf, source, "database", fields, testAgentNow))

	return buf.String()
}

func TestBlockHeaderPlacesCallerFieldsBetweenScopeAndTimestamp(t *testing.T) {
	header := blockHeader(t, "pg_stat_user_tables", []headerField{
		{"db", "orders_db"},
		{"dbid", "16401"},
		{"sample", "1"},
		{"truncated", "false"},
	})

	assert.Equal(t, []string{
		"#",
		"engine=postgres",
		"source=pg_stat_user_tables",
		"v=1",
		"format=csv",
		"scope=database",
		"db=orders_db",
		"dbid=16401",
		"sample=1",
		"truncated=false",
		"ts=2026-08-04T09:12:44.118Z",
	}, strings.Fields(header))
}

func TestBlockHeaderWithoutFieldsHasNoStraySeparator(t *testing.T) {
	assert.Equal(t,
		"# engine=postgres source=pg_bloat v=1 format=csv scope=database ts=2026-08-04T09:12:44.118Z\n",
		blockHeader(t, "pg_bloat", nil))

	assert.Equal(t, blockHeader(t, "pg_bloat", nil),
		blockHeader(t, "pg_bloat", []headerField{}),
		"an empty field list renders as no fields, not as an empty token")
}

func TestBlockHeaderKeepsKeysWithEmptyValues(t *testing.T) {
	assert.Contains(t, blockHeader(t, "pg_bloat", []headerField{
		{"db", "orders_db"},
		{"dbid", ""},
	}), " db=orders_db dbid= ts=")
}

func TestBlockHeaderValueQuoting(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"classified token stays bare", "too_many_connections", "too_many_connections"},
		{"identifier stays bare", "orders_line_items", "orders_line_items"},
		{"number stays bare", "16401", "16401"},
		{"empty stays empty", "", ""},
		{
			"driver text is quoted",
			"ERROR: canceling statement due to statement timeout",
			`"ERROR: canceling statement due to statement timeout"`,
		},
		{"a double quote is quoted and escaped", `orders"tbl`, `"orders\"tbl"`},
		{"a tab is quoted", "a\tb", `"a\tb"`},
		{"a newline is flattened first, then quoted", "line one\nline two", `"line one line two"`},
		{"a control character is quoted", "a\x00b", `"a\x00b"`},
		{

			"non-ASCII without whitespace stays bare and readable",
			"注文テーブル",
			"注文テーブル",
		},
		{
			"non-ASCII with whitespace is quoted but stays readable",
			"注文 テーブル",
			`"注文 テーブル"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, headerValue(tt.value))
		})
	}
}

func TestBlockHeaderTruncatesLongValues(t *testing.T) {
	long := "ERROR: " + strings.Repeat("detail ", 200)

	got := headerValue(long)
	unquoted, err := strconv.Unquote(got)
	require.NoError(t, err, "a truncated value is still a well-formed quoted token")

	assert.Len(t, []rune(unquoted), maxHeaderValue+len("..."))
	assert.True(t, strings.HasSuffix(unquoted, "..."), "the cut is marked, not silent")
	assert.True(t, strings.HasPrefix(unquoted, "ERROR: detail"), "the head of the message survives")
}

func TestBlockHeaderTruncatesOnRuneBoundaries(t *testing.T) {
	got := headerValue(strings.Repeat("é", maxHeaderValue+50))

	assert.True(t, utf8.ValidString(got))
	assert.Equal(t, strings.Repeat("é", maxHeaderValue)+"...", got)
}

func TestWriteRowsWithNoRowsEmitsColumnHeaderOnly(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRows(&buf, []string{"relid", "relname"}, nil))

	assert.Equal(t, "relid,relname\n", buf.String())
}

func TestWriteRowsFlattensEveryCell(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeRows(&buf,
		[]string{"relid", "relname"},
		[][]string{
			{"16390", "orders\nwith a break"},
			{"16482", "orders,with a comma"},
		},
	))

	assert.Equal(t, strings.Join([]string{
		"relid,relname",
		"16390,orders with a break",
		`16482,"orders,with a comma"`,
		"",
	}, "\n"), buf.String())

	assert.Len(t, strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n"), 3)
}

func TestBlockHeaderErrorsPropagate(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	assert.ErrorIs(t,
		writeBlockHeader(failingWriter{err: sinkErr}, "pg_bloat", "database", nil, testAgentNow),
		sinkErr)
}

func TestWriteRowsErrorsPropagate(t *testing.T) {
	sinkErr := errors.New("no space left on device")

	assert.ErrorIs(t,
		writeRows(failingWriter{err: sinkErr}, []string{"relid"}, [][]string{{"16390"}}),
		sinkErr)
}
