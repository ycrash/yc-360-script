//go:build pgintegration

package postgres

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type matrixServer struct {
	major int
	port  int
}

var matrixServers = []matrixServer{
	{major: 14, port: 15414},
	{major: 15, port: 15415},
	{major: 16, port: 15416},
	{major: 17, port: 15417},
	{major: 18, port: 15418},
}

type matrixRole struct {
	user     string
	password string

	superuser bool
	monitor   bool
}

var matrixRoles = []matrixRole{
	{user: "postgres", password: "yc-superuser-pw", superuser: true},
	{user: "yc_monitor", password: "yc-monitor-pw", monitor: true},
	{user: "yc_restricted", password: "yc-restricted-pw"},
}

func (r matrixRole) privileged() bool { return r.superuser || r.monitor }

var superuserOnlySettings = []string{
	"data_directory",
	"log_directory",
	"log_filename",
	"shared_preload_libraries",
}

func matrixHost() string {
	if host := os.Getenv("YC_PG_MATRIX_HOST"); host != "" {
		return host
	}

	return "127.0.0.1"
}

func matrixTarget(server matrixServer, role matrixRole) Target {
	return Target{
		Host:     matrixHost(),
		Port:     server.port,
		Database: "postgres",
		Username: role.user,
		Password: role.password,

		SSLMode: "disable",
	}
}

func collectFromMatrix(t *testing.T, target Target) Metadata {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	if err != nil {
		t.Fatalf("connect to %s: %v\n\nIs the matrix running? docker compose -f compose.pg.yaml up -d --wait", target, err)
	}

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		assert.NoError(t, conn.Close(closeCtx))
	}()

	return Collect(ctx, conn, target, time.Now())
}

func TestMatrix(t *testing.T) {
	for _, server := range matrixServers {
		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)
				m := collectFromMatrix(t, target)

				values := assertArtifactComplete(t, target, m)

				assertServerFacts(t, server, role, values)
				assertCapabilities(t, server, values)
				assertSettingsVisibility(t, role, values)
				assertLogLocation(t, server, role, values)
				assertSameHost(t, server, role, values)
				assertReplicationProbe(t, values)
			})
		}
	}
}

const matrixCaptureSessionsSQL = `SELECT count(*)
FROM pg_catalog.pg_stat_activity
WHERE application_name = $1 AND pid <> pg_backend_pid()`

const matrixMetadataWindow = 3 * time.Second

func runMatrixMetadataWindow(t *testing.T, target Target) ([]ArtifactResult, int64) {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration: matrixMetadataWindow,
		Target:   target,
		Collectors: []Collector{
			NewMetadata(target, "matrix", time.Now(), ""),
			Health{Interval: time.Second},
		},
	}

	var (
		wg       sync.WaitGroup
		sessions int64
		countErr error
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		time.Sleep(matrixMetadataWindow / 3)
		sessions, countErr = matrixCountCaptureSessions(target)
	}()

	results := window.Run(context.Background())
	wg.Wait()

	require.NoError(t, countErr)

	return results, sessions
}

func matrixCountCaptureSessions(target Target) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	if err != nil {
		return 0, err
	}

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	var sessions int64
	if err := conn.QueryRow(ctx, matrixCaptureSessionsSQL, ApplicationName).Scan(&sessions); err != nil {
		return 0, err
	}

	return sessions, nil
}

func TestMatrixMetadataWindow(t *testing.T) {
	for _, server := range matrixServers {
		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)

				direct := collectFromMatrix(t, target)

				results, sessions := runMatrixMetadataWindow(t, target)
				require.Len(t, results, 2)
				require.NoError(t, results[0].IOErr)

				require.Equal(t, StatusComplete, results[0].Status,
					"the capability read must complete for every role on every version")
				require.Equal(t, 1, results[0].SamplesWritten, "Once() is one reading")

				assert.EqualValues(t, 1, sessions,
					"two artifacts, one connection: before this slice the metadata capture "+
						"dialled a second one of its own")

				content, err := os.ReadFile(results[0].Artifact.FileName)
				require.NoError(t, err)
				results[0].File.Close()
				results[1].File.Close()

				artifact := string(content)
				assert.NotContains(t, artifact, target.Password, "the artifact carries the password")

				headers, values, keys := parseArtifact(t, artifact)

				require.Len(t, headers, 4, "preamble, target block, server block, closing block")
				for i, source := range []string{
					"source=pg_metadata ",
					"source=pg_metadata_target ",
					"source=pg_metadata_server ",
					"source=pg_metadata ",
				} {
					assert.Contains(t, headers[i], source, "block %d", i)
				}

				_, directValues, directKeys := parseArtifact(t, writeArtifact(t, direct))
				assert.Equal(t, directKeys, keys,
					"the window writes exactly the keys the direct call does")

				assert.Equal(t, directValues["log_access"], values["log_access"],
					"and reaches the same conclusion about the mode by either route")

				assert.Equal(t, "matrix", values["yc360_version"],
					"the version stamped before the connection survives Collect's fresh value")
				assert.Equal(t, role.user, values["current_user"])
				assert.NotContains(t, values, "connect_error",
					"the key exists only in the closing block's header")
			})
		}
	}
}

func assertArtifactComplete(t *testing.T, target Target, m Metadata) map[string]string {
	t.Helper()

	m.YC360Version = "matrix"

	artifact := writeArtifact(t, m)

	assert.NotContains(t, artifact, target.Password, "the artifact carries the password")

	_, values, keys := parseArtifact(t, artifact)

	_, _, wantKeys := parseArtifact(t, golden(t, "pg_metadata_full.txt"))
	require.Equal(t, wantKeys, keys,
		"the key set must not depend on the server version or the role's privileges")

	assert.Empty(t, values["connect_error"], "a collected capture has no connect error")

	return values
}

func assertServerFacts(t *testing.T, server matrixServer, role matrixRole, values map[string]string) {
	t.Helper()

	assert.Empty(t, values["query_error"],
		"serverFactsSQL must succeed for every role, including one holding only LOGIN")

	assert.Equal(t, "postgres", values["current_database"])
	assert.Equal(t, role.user, values["current_user"])
	assert.Equal(t, "false", values["is_in_recovery"])
	assert.Contains(t, values["version"], "PostgreSQL")

	versionNum, err := strconv.Atoi(values["server_version_num"])
	require.NoError(t, err, "server_version_num must be an integer")
	assert.GreaterOrEqual(t, versionNum, server.major*10000)
	assert.Less(t, versionNum, (server.major+1)*10000)

	for _, key := range []string{
		"backend_pid",
		"postmaster_start_time",
		"server_now",
		"server_clock_timestamp",
		"agent_ts_at_clock_read",
		"max_connections",
		"compute_query_id",
	} {
		assert.NotEmpty(t, values[key], "%s is readable by every role", key)
	}

	assert.NotEmpty(t, values["inet_server_addr"])

	assert.Equal(t, "5432", values["inet_server_port"])
	assert.NotEqual(t, strconv.Itoa(server.port), values["inet_server_port"])

	assert.Equal(t, strconv.FormatBool(role.superuser || role.monitor), values["has_pg_monitor_role"],
		"pg_has_role(..., 'member') is true for a superuser as well as for a member")

	assert.Equal(t, strconv.FormatBool(role.superuser || role.monitor), values["has_pg_read_all_stats"],
		"the gate pg_stat_activity actually applies, where pg_monitor merely includes it - a "+
			"role granted pg_read_all_stats directly reads everything while the monitor flag "+
			"says false. Probed with 'usage' rather than 'member', because since PostgreSQL 15 "+
			"the server gates on privilege inheritance and a NOINHERIT member under 'member' "+
			"reads true while every foreign query is masked - a divergence the three matrix "+
			"roles cannot exhibit, since all of them are INHERIT")

	querySize, err := strconv.Atoi(values["track_activity_query_size"])
	assert.NoError(t, err,
		"pg_settings.setting is raw internal units, so this is a number of bytes and never "+
			"SHOW's 1kB - which is what lets a report tell a server-truncated query from a "+
			"malformed one")
	assert.Positive(t, querySize)
}

func assertCapabilities(t *testing.T, server matrixServer, values map[string]string) {
	t.Helper()

	assert.Equal(t, strconv.FormatBool(server.major >= 17), values["has_pg_stat_checkpointer"],
		"pg_stat_checkpointer landed in PostgreSQL 17")

	assert.Equal(t, "true", values["has_session_fatal_stats"])

	assert.Equal(t, "true", values["has_pg_stat_statements"])
	assert.NotEmpty(t, values["pg_stat_statements_version"])
	t.Logf("pg%d: pg_stat_statements %s, server_version_num %s",
		server.major, values["pg_stat_statements_version"], values["server_version_num"])
}

func assertSettingsVisibility(t *testing.T, role matrixRole, values map[string]string) {
	t.Helper()

	assert.Equal(t, "on", values["logging_collector"])
	assert.NotEmpty(t, values["log_destination"])
	assert.NotEmpty(t, values["log_line_prefix"])

	// auto_explain's GUCs exist only while the module is loaded, and the fixture does not
	// preload it, so they are absent for every role - what a customer running without the
	// module sees too.
	if role.privileged() {
		assert.ElementsMatch(t, autoExplainSettings,
			splitSettingList(values["settings_unavailable"]),
			"a role with pg_read_all_settings sees every setting that exists; what is "+
				"missing is the unloaded module's")

		for _, name := range superuserOnlySettings {
			assert.NotEmpty(t, values[name], "%s is visible to this role", name)
		}

		assert.Contains(t, values["shared_preload_libraries"], "pg_stat_statements")
		assert.NotContains(t, values["shared_preload_libraries"], "auto_explain",
			"and the fixture says why those five are missing")

		return
	}

	unavailable := splitSettingList(values["settings_unavailable"])
	assert.ElementsMatch(t, append(slices.Clone(superuserOnlySettings), autoExplainSettings...),
		unavailable,
		"the superuser-only settings and the unloaded module's, and they are all named")

	for _, name := range superuserOnlySettings {
		assert.Empty(t, values[name], "%s must be written empty rather than erroring the statement", name)
	}
}

func matrixFunctionAllowed(server matrixServer, role matrixRole) bool {
	return role.superuser || (role.monitor && server.major >= 17)
}

func assertSameHost(t *testing.T, server matrixServer, role matrixRole, values map[string]string) {
	t.Helper()

	verdict := values["agent_on_db_host"]
	reason := values["agent_on_db_host_reason"]

	t.Logf("pg%d/%s: agent_on_db_host=%s by=%s reason=%s evidence=%q",
		server.major, role.user, verdict, values["agent_on_db_host_by"],
		reason, values["agent_on_db_host_evidence"])

	require.Contains(t, []string{OnDBHostYes, OnDBHostNo, OnDBHostUnknown}, verdict,
		"the verdict is always one of the three")

	if verdict == OnDBHostYes {
		assert.Empty(t, reason, "a yes carries no reason")
		assert.NotEmpty(t, values["agent_on_db_host_by"], "a yes always names the test behind it")

		return
	}

	assert.NotEmpty(t, reason, "%s must always be paired with a reason", verdict)
	assert.Empty(t, values["agent_on_db_host_by"], "only a yes names the test that produced it")

	// The suite connects to a container over TCP, so a yes from a title match on a
	// PID belonging to another namespace would be wrong.
	assert.NotEqual(t, confirmedByBackendPID, values["agent_on_db_host_by"])
}

func assertLogLocation(t *testing.T, server matrixServer, role matrixRole, values map[string]string) {
	t.Helper()

	t.Logf("pg%d/%s: log_access=%s reason=%s resolved_by=%s current_logfile=%q",
		server.major, role.user, values["log_access"], values["log_access_reason"],
		values["log_resolved_by"], values["current_logfile"])

	if !role.privileged() {
		assert.Equal(t, LogAccessNone, values["log_access"],
			"no route produced a path, which is remote rather than unknown")
		assert.Empty(t, values["log_resolved_by"])
		assert.Equal(t, reasonUnresolved, values["log_access_reason"],
			"pg_current_logfile() is denied to a bare LOGIN role on every supported version, "+
				"so no route produced a path; the denial text itself is now agent-log only")

		return
	}

	want := resolvedByGlob
	if matrixFunctionAllowed(server, role) {
		want = resolvedByFunction
	}

	assert.Equal(t, want, values["log_resolved_by"])

	assert.Equal(t, LogAccessDirect, values["log_access"],
		"before this slice, pg%d with this role reported pg-remote from the database host itself",
		server.major)

	assert.NotEmpty(t, values["current_logfile_resolved"])
	assert.Empty(t, values["log_access_reason"], "direct access leaves the reason empty")
	assert.Equal(t, "stderr", values["log_formats"])

	assert.NotEmpty(t, values["data_directory"],
		"data_directory rides in the settings catalogue whatever the function does")
}

func assertReplicationProbe(t *testing.T, values map[string]string) {
	t.Helper()

	assert.Empty(t, values["replication_probe_error"],
		"count(*) on pg_stat_replication needs no grant")
	assert.Equal(t, "false", values["replication_configured"],
		"a standalone container has no replicas")
}

func splitSettingList(value string) []string {
	if value == "" {
		return nil
	}

	return strings.Split(value, ",")
}

const (
	matrixCountedTables    = 2
	matrixPartitionedRelns = 3
	matrixBulkTables       = 200

	matrixUserTables = matrixCountedTables + matrixPartitionedRelns + matrixBulkTables

	matrixOrdersInserted = 500
	matrixOrdersDead     = 100
	matrixOrdersLive     = matrixOrdersInserted - matrixOrdersDead
	matrixNoIdxLive      = 250
)

type sampleBlock struct {
	header  map[string]string
	rawHead string
	columns []string
	rows    map[string][]string
}

func parseBloatBlocks(t *testing.T, artifact string) []sampleBlock {
	t.Helper()

	return parseSampleBlocks(t, artifact, "pg_stat_user_tables")
}

func parseHealthBlocks(t *testing.T, artifact string) []sampleBlock {
	t.Helper()

	return parseSampleBlocks(t, artifact, "pg_stat_database")
}

func parseSampleBlocks(t *testing.T, artifact, source string) []sampleBlock {
	t.Helper()

	var (
		blocks  []sampleBlock
		current *sampleBlock
	)

	for _, line := range strings.Split(strings.TrimSuffix(artifact, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			current = nil

			if !strings.Contains(line, "source="+source+" ") {
				continue
			}

			header := map[string]string{}
			for _, token := range strings.Fields(strings.TrimPrefix(line, "# ")) {
				key, value, found := strings.Cut(token, "=")
				if found {
					header[key] = value
				}
			}

			blocks = append(blocks, sampleBlock{
				header: header, rawHead: line, rows: map[string][]string{},
			})
			current = &blocks[len(blocks)-1]

			continue
		}

		if current == nil {
			continue
		}

		cells := strings.Split(line, ",")
		if current.columns == nil {
			current.columns = cells
			continue
		}

		current.rows[cells[0]] = cells
	}

	return blocks
}

func (b sampleBlock) cell(t *testing.T, relid, column string) string {
	t.Helper()

	row, ok := b.rows[relid]
	require.True(t, ok, "relid %s is not in the block", relid)

	for i, name := range b.columns {
		if name == column {
			return row[i]
		}
	}

	t.Fatalf("column %s is not in the block", column)

	return ""
}

// schema is matched, not assumed: two schemas can hold same-named tables, and a lookup
// that ignored it would return whichever the map iterated first.
//
//nolint:unparam // every fixture table happens to live in public; the match is the point
func (b sampleBlock) relidOf(t *testing.T, schema, name string) string {
	t.Helper()

	for relid, row := range b.rows {
		if row[1] == schema && row[2] == name {
			return relid
		}
	}

	t.Fatalf("%s.%s is not in the block", schema, name)

	return ""
}

func runMatrixBloatWindow(t *testing.T, target Target) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{

		Duration:   time.Second,
		Target:     target,
		Collectors: []Collector{Bloat{}},
	}

	return window.Run(context.Background())
}

func TestMatrixBloat(t *testing.T) {
	for _, server := range matrixServers {
		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)

				results := runMatrixBloatWindow(t, target)
				require.Len(t, results, 1)
				require.NoError(t, results[0].IOErr)

				require.Equal(t, StatusComplete, results[0].Status,
					"the artifact must be complete for every role on every version")
				require.Equal(t, 2, results[0].SamplesWritten)

				content, err := os.ReadFile(results[0].Artifact.FileName)
				require.NoError(t, err)
				results[0].File.Close()

				artifact := string(content)
				assert.NotContains(t, artifact, target.Password, "the artifact carries the password")

				blocks := parseBloatBlocks(t, artifact)
				require.Len(t, blocks, 2, "start and end")

				assert.Equal(t, bloatColumns, blocks[0].columns)
				assert.Equal(t, blocks[0].columns, blocks[1].columns,
					"both samples carry identical column sets")

				assert.Equal(t, rowKeys(blocks[0]), rowKeys(blocks[1]),
					"relid is stable across the two samples")

				for i, block := range blocks {
					assert.Equal(t, strconv.Itoa(matrixUserTables), block.header["tables_total"],
						"block %d: tables_total must match the fixture", i)
					assert.Equal(t, strconv.Itoa(matrixUserTables), block.header["tables_written"],
						"block %d: nothing is dropped below the cap", i)
					assert.Equal(t, "false", block.header["truncated"])

					assert.NotContains(t, block.rawHead, "sizes=unavailable",
						"block %d: pg_table_size/pg_indexes_size must need no grant", i)
				}

				assertMatrixKnownTables(t, blocks[1])
				assertMatrixCapFires(t, target)
			})
		}
	}
}

func assertMatrixDeadTuples(t *testing.T, block sampleBlock, orders string) {
	t.Helper()

	updated, err := strconv.Atoi(block.cell(t, orders, "n_tup_upd"))
	require.NoError(t, err, "n_tup_upd must be a number")

	dead, err := strconv.Atoi(block.cell(t, orders, "n_dead_tup"))
	require.NoError(t, err, "n_dead_tup must be a number")

	assert.GreaterOrEqual(t, dead, matrixOrdersDead,
		"autovacuum is disabled on the fixture, so the %d tuples its DELETE made dead "+
			"are still here", matrixOrdersDead)

	assert.LessOrEqual(t, dead, matrixOrdersDead+updated,
		"every dead tuple is accounted for by the fixture's DELETE or by an UPDATE the "+
			"block itself reports")
}

func assertMatrixKnownTables(t *testing.T, block sampleBlock) {
	t.Helper()

	orders := block.relidOf(t, "public", "yc_bloat_orders")
	noIndexes := block.relidOf(t, "public", "yc_bloat_no_indexes")

	assert.Equal(t, strconv.Itoa(matrixOrdersLive), block.cell(t, orders, "n_live_tup"))
	assertMatrixDeadTuples(t, block, orders)
	assert.Equal(t, strconv.Itoa(matrixNoIdxLive), block.cell(t, noIndexes, "n_live_tup"))

	assert.Equal(t, "", block.cell(t, noIndexes, "idx_scan"),
		"a table with no indexes reports NULL, which must be written empty")
	assert.NotEqual(t, "", block.cell(t, orders, "idx_scan"),
		"an indexed table reports a number, even when it is 0")

	assert.Equal(t, "", block.cell(t, noIndexes, "last_autovacuum"))
	assert.Equal(t, "", block.cell(t, noIndexes, "last_vacuum"))

	assert.NotEmpty(t, block.cell(t, orders, "table_size_bytes"))
	assert.NotEmpty(t, block.cell(t, orders, "index_size_bytes"))
	assert.NotEmpty(t, block.cell(t, noIndexes, "table_size_bytes"))
	assert.Equal(t, "0", block.cell(t, noIndexes, "index_size_bytes"))

	parent := block.relidOf(t, "public", "yc_bloat_parted")
	block.relidOf(t, "public", "yc_bloat_parted_p1")
	block.relidOf(t, "public", "yc_bloat_parted_p2")

	assert.Equal(t, "0", block.cell(t, parent, "table_size_bytes"),
		"a relation with no storage is 0, not empty: empty stays reserved for one that vanished")
	assert.Equal(t, "0", block.cell(t, parent, "index_size_bytes"))
}

func assertMatrixCapFires(t *testing.T, target Target) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		assert.NoError(t, conn.Close(closeCtx))
	}()

	const capped = 10

	var buf strings.Builder
	require.NoError(t, Bloat{MaxTables: capped}.Sample(ctx, conn, &buf, SampleContext{
		At: time.Now(), Index: 1, Database: "postgres", DBID: "0",
	}))

	blocks := parseBloatBlocks(t, buf.String())
	require.Len(t, blocks, 1)

	assert.Equal(t, strconv.Itoa(capped), blocks[0].header["tables_written"])
	assert.Equal(t, strconv.Itoa(matrixUserTables), blocks[0].header["tables_total"],
		"the real total survives the cap, so a capped file cannot read as complete")
	assert.Equal(t, "true", blocks[0].header["truncated"])
	assert.Len(t, blocks[0].rows, capped)
}

const matrixHealthSamples = 3

const matrixDatabases = 5

func runMatrixHealthWindow(t *testing.T, target Target) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration:   time.Duration(matrixHealthSamples) * time.Second,
		Target:     target,
		Collectors: []Collector{Health{Interval: time.Second}},
	}

	return window.Run(context.Background())
}

func TestMatrixHealth(t *testing.T) {
	for _, server := range matrixServers {
		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)

				stop := matrixKeepCommitting(t, target)
				results := runMatrixHealthWindow(t, target)
				stop()

				require.Len(t, results, 1)
				require.NoError(t, results[0].IOErr)

				require.Equal(t, StatusComplete, results[0].Status,
					"pg_stat_database masks nothing, so the artifact is complete for every role")
				require.Equal(t, matrixHealthSamples, results[0].SamplesWritten)

				content, err := os.ReadFile(results[0].Artifact.FileName)
				require.NoError(t, err)
				results[0].File.Close()

				artifact := string(content)
				assert.NotContains(t, artifact, target.Password, "the artifact carries the password")

				blocks := parseHealthBlocks(t, artifact)
				require.Len(t, blocks, matrixHealthSamples)

				connected := blocks[0].header["dbid"]
				require.NotEmpty(t, connected, "identify read no OID for the connected database")

				for i, block := range blocks {
					assert.Equal(t, healthColumns, block.columns,
						"block %d: every sample carries an identical column set", i)

					assert.NotContains(t, block.rawHead, "sessions_fatal=unavailable",
						"block %d: sessions_fatal exists on 14-18, so the 42703 path is dead code "+
							"across the supported range", i)

					assert.Equal(t, strconv.Itoa(matrixDatabases), block.header["databases_total"])
					assert.Equal(t, strconv.Itoa(matrixDatabases), block.header["databases_written"])
					assert.Equal(t, "false", block.header["truncated"])

					assert.Equal(t, "", block.cell(t, "0", "datname"),
						"block %d: the shared-objects row accounts for shared relations, not a "+
							"database, and its NULL datname is written empty", i)

					second := matrixDatidOf(t, block, "yc_second")
					assert.NotEqual(t, connected, second,
						"block %d: the capture is connected to postgres and sees yc_second anyway - "+
							"unfiltered, observed rather than argued", i)
				}

				assert.Equal(t, rowKeys(blocks[0]), rowKeys(blocks[len(blocks)-1]),
					"datid is stable across the samples, which is what the server merges on")

				first := matrixCounter(t, blocks[0], connected, "xact_commit")
				last := matrixCounter(t, blocks[len(blocks)-1], connected, "xact_commit")
				assert.Greater(t, last, first,
					"xact_commit for the connected database must climb across the window")

				assertMatrixHealthCapKeepsTheProtectedRows(t, target)
			})
		}
	}
}

func matrixKeepCommitting(t *testing.T, target Target) (stop func()) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		for ctx.Err() == nil {
			var one int
			if err := conn.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
				return
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()

	return func() {
		cancel()
		<-done

		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}
}

func assertMatrixHealthCapKeepsTheProtectedRows(t *testing.T, target Target) {
	t.Helper()

	target.Database = "yc_second"

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		assert.NoError(t, conn.Close(closeCtx))
	}()

	var (
		database              string
		dbid                  *string
		hasPgStatCheckpointer bool
		hasGenericPlan        bool
	)
	require.NoError(t, conn.QueryRow(ctx, currentDatabaseSQL).
		Scan(&database, &dbid, &hasPgStatCheckpointer, &hasGenericPlan))
	require.Equal(t, "yc_second", database)
	require.NotNil(t, dbid)

	const capped = 2

	var buf strings.Builder
	require.NoError(t, Health{MaxDatabases: capped}.Sample(ctx, conn, &buf, SampleContext{
		At: time.Now(), Index: 1, Database: database, DBID: *dbid,
	}))

	blocks := parseHealthBlocks(t, buf.String())
	require.Len(t, blocks, 1)

	assert.Equal(t, strconv.Itoa(capped), blocks[0].header["databases_written"])
	assert.Equal(t, strconv.Itoa(matrixDatabases), blocks[0].header["databases_total"],
		"the real total survives the cap, so a capped block cannot read as complete")
	assert.Equal(t, "true", blocks[0].header["truncated"])

	assert.Equal(t, []string{"0", *dbid}, rowKeys(blocks[0]),
		"the shared row and the connected database survive - yc_second has the highest OID, "+
			"so ordering on datid alone would have dropped the database the header names")
}

func matrixDatidOf(t *testing.T, block sampleBlock, datname string) string {
	t.Helper()

	for datid, row := range block.rows {
		if row[1] == datname {
			return datid
		}
	}

	t.Fatalf("database %s is not in the block", datname)

	return ""
}

func matrixCounter(t *testing.T, block sampleBlock, datid, column string) int64 {
	t.Helper()

	value, err := strconv.ParseInt(block.cell(t, datid, column), 10, 64)
	require.NoError(t, err, "%s must be a number", column)

	return value
}

func rowKeys(b sampleBlock) []string {
	relids := make([]string, 0, len(b.rows))
	for relid := range b.rows {
		relids = append(relids, relid)
	}

	sort.Strings(relids)

	return relids
}

const matrixCapacityWindow = 3 * time.Second

const matrixHeldSessions = 3

type capacityMatrixBlock struct {
	header  map[string]string
	rawHead string
	columns []string
	rows    [][]string
}

func parseCapacityBlocks(t *testing.T, artifact, source string) []capacityMatrixBlock {
	t.Helper()

	var (
		blocks  []capacityMatrixBlock
		current *capacityMatrixBlock
	)

	for _, line := range strings.Split(strings.TrimSuffix(artifact, "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			current = nil

			if !strings.Contains(line, "source="+source+" ") {
				continue
			}

			header := map[string]string{}
			for _, token := range strings.Fields(strings.TrimPrefix(line, "# ")) {
				if key, value, found := strings.Cut(token, "="); found {
					header[key] = value
				}
			}

			blocks = append(blocks, capacityMatrixBlock{header: header, rawHead: line})
			current = &blocks[len(blocks)-1]

			continue
		}

		if current == nil {
			continue
		}

		cells := parseCSVRecord(t, line)

		if current.columns == nil {
			current.columns = cells
			continue
		}

		current.rows = append(current.rows, cells)
	}

	return blocks
}

func parseCSVRecord(t *testing.T, line string) []string {
	t.Helper()

	reader := csv.NewReader(strings.NewReader(line))
	reader.FieldsPerRecord = -1

	record, err := reader.Read()
	require.NoError(t, err, "not a CSV record: %s", line)

	return record
}

func (b capacityMatrixBlock) index(t *testing.T, column string) int {
	t.Helper()

	for i, name := range b.columns {
		if name == column {
			return i
		}
	}

	t.Fatalf("column %s is not in the block", column)

	return -1
}

func (b capacityMatrixBlock) only(t *testing.T, column string) string {
	t.Helper()

	require.Len(t, b.rows, 1, "%s is a one-row block", b.header["source"])

	return b.rows[0][b.index(t, column)]
}

func runMatrixCapacityWindow(t *testing.T, target Target, between func()) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration:   matrixCapacityWindow,
		Target:     target,
		Collectors: []Collector{Capacity{}},
	}

	var wg sync.WaitGroup

	if between != nil {
		wg.Add(1)

		go func() {
			defer wg.Done()

			time.Sleep(matrixCapacityWindow / 3)
			between()
		}()
	}

	results := window.Run(context.Background())
	wg.Wait()

	return results
}

func TestMatrixCapacity(t *testing.T) {
	for _, server := range matrixServers {
		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)

				release := matrixHoldSessions(t, target, matrixHeldSessions)

				var between func()
				if role.superuser {
					between = func() { matrixExec(t, target, "CHECKPOINT") }
				}

				results := runMatrixCapacityWindow(t, target, between)
				release()

				require.Len(t, results, 1)
				require.NoError(t, results[0].IOErr)

				require.Equal(t, StatusComplete, results[0].Status,
					"a denied read costs its own block, never the artifact")
				require.Equal(t, 2, results[0].SamplesWritten)

				artifact := matrixArtifactText(t, results[0])
				assert.NotContains(t, artifact, target.Password, "the artifact carries the password")

				checkpoints := parseCapacityBlocks(t, artifact, "pg_checkpointer")
				require.Len(t, checkpoints, 2, "the counters are read at both edges of the window")

				assertMatrixCheckpointShape(t, server, checkpoints)
				assertMatrixCheckpointCounters(t, role, checkpoints)

				connections := parseCapacityBlocks(t, artifact, "pg_stat_activity_by_app")
				require.Len(t, connections, 1, "a gauge, written once as the window closes")
				assertMatrixConnectionVisibility(t, role, connections[0])

				wal := parseCapacityBlocks(t, artifact, "pg_ls_waldir")
				require.Len(t, wal, 1)
				assertMatrixWAL(t, role, wal[0])

				if role.superuser {
					assertMatrixResetClocksAreTwo(t, server, target)
				}
			})
		}
	}
}

func assertMatrixCheckpointShape(t *testing.T, server matrixServer, blocks []capacityMatrixBlock) {
	t.Helper()

	views := "pg_stat_bgwriter"
	if server.major >= 17 {
		views = "pg_stat_checkpointer,pg_stat_bgwriter"
	}

	for i, block := range blocks {
		assert.NotContains(t, block.rawHead, "error=",
			"block %d: reading the checkpoint counters needs no grant", i)

		assert.Equal(t, checkpointColumns, block.columns,
			"block %d: one column set on every version is what the normalisation buys", i)
		assert.Equal(t, views, block.header["views"],
			"block %d: three of the five counters moved views in 17, and views= is where the "+
				"artifact says which server it read", i)

		if server.major >= 17 {
			assert.Empty(t, block.only(t, "buffers_backend"),
				"block %d: the column was removed in 17, and empty is not 0", i)
		} else {
			assert.NotEmpty(t, block.only(t, "buffers_backend"),
				"block %d: below 17 it is a reading", i)
		}

		assert.NotEmpty(t, block.only(t, "buffers_clean"),
			"block %d: the one counter that stayed in pg_stat_bgwriter", i)
		assert.NotEmpty(t, block.only(t, "checkpointer_stats_reset"), "block %d", i)
		assert.NotEmpty(t, block.only(t, "bgwriter_stats_reset"), "block %d", i)
	}
}

func assertMatrixCheckpointCounters(t *testing.T, role matrixRole, blocks []capacityMatrixBlock) {
	t.Helper()

	first := matrixCheckpointCounter(t, blocks[0], "checkpoints_req")
	last := matrixCheckpointCounter(t, blocks[1], "checkpoints_req")

	if role.superuser {
		assert.Greater(t, last, first,
			"the run issued a CHECKPOINT between the two samples, so the requested count climbs")

		return
	}

	assert.GreaterOrEqual(t, last, first,
		"an unprivileged role cannot force a checkpoint, so only the direction is assertable")
}

func matrixCheckpointCounter(t *testing.T, block capacityMatrixBlock, column string) int64 {
	t.Helper()

	value, err := strconv.ParseInt(block.only(t, column), 10, 64)
	require.NoError(t, err, "%s must be a number", column)

	return value
}

func assertMatrixConnectionVisibility(t *testing.T, role matrixRole, block capacityMatrixBlock) {
	t.Helper()

	assert.NotContains(t, block.rawHead, "error=", "counting pg_stat_activity needs no grant")
	assert.Equal(t, connectionColumns, block.columns)
	assert.Equal(t, "false", block.header["truncated"])

	assert.GreaterOrEqual(t, matrixConnectionCount(t, block, ApplicationName, "client backend"),
		int64(matrixHeldSessions+1),
		"every role sees its own backends in full: the window's connection and the %d held "+
			"open beside it, under the agent's own application_name",
		matrixHeldSessions)

	checkpointer := matrixConnectionCount(t, block, "", "checkpointer")
	masked := matrixConnectionCount(t, block, "", "")

	if role.privileged() {
		assert.Positive(t, checkpointer,
			"a privileged role sees the server's own processes as their own backend_type - which "+
				"is what keeps them out of a client-connection count read against max_connections")
		assert.Positive(t, matrixConnectionCount(t, block, "", "walwriter"))
		assert.Zero(t, masked, "and nothing is masked into an unnamed, untyped row")

		return
	}

	assert.Zero(t, checkpointer, "an unprivileged role cannot see another backend's type")
	assert.GreaterOrEqual(t, masked, int64(2),
		"and the backends it cannot see collapse into one row with both dimensions empty")
}

func matrixConnectionCount(t *testing.T, block capacityMatrixBlock, application, backendType string) int64 {
	t.Helper()

	var (
		name  = block.index(t, "application_name")
		kind  = block.index(t, "backend_type")
		count = block.index(t, "active_connections")
	)

	for _, row := range block.rows {
		if row[name] != application || row[kind] != backendType {
			continue
		}

		value, err := strconv.ParseInt(row[count], 10, 64)
		require.NoError(t, err, "active_connections must be a number")

		return value
	}

	return 0
}

func assertMatrixWAL(t *testing.T, role matrixRole, block capacityMatrixBlock) {
	t.Helper()

	assert.Equal(t, walColumns, block.columns,
		"the column header is written whether or not the read succeeded")

	if role.privileged() {
		assert.NotContains(t, block.rawHead, "error=",
			"pg_ls_waldir() is granted to pg_monitor, so the recommended role reads it")

		value, err := strconv.ParseInt(block.only(t, "wal_bytes"), 10, 64)
		require.NoError(t, err, "wal_bytes must be a number")
		assert.Positive(t, value, "a running server always has WAL")

		return
	}

	assert.Contains(t, block.rawHead, "error=", "a role holding only LOGIN is denied")
	assert.Contains(t, block.rawHead, "pg_ls_waldir")
	assert.Contains(t, block.rawHead, "42501")
	assert.Empty(t, block.rows,
		"the column header with no row: captured nothing, and the header says why")
}

func assertMatrixResetClocksAreTwo(t *testing.T, server matrixServer, target Target) {
	t.Helper()

	results := runMatrixCapacityWindow(t, target, func() {
		matrixExec(t, target, "SELECT pg_stat_reset_shared('bgwriter')")
	})

	require.Len(t, results, 1)
	require.NoError(t, results[0].IOErr)

	blocks := parseCapacityBlocks(t, matrixArtifactText(t, results[0]), "pg_checkpointer")
	require.Len(t, blocks, 2)

	assert.NotEqual(t, blocks[0].only(t, "bgwriter_stats_reset"), blocks[1].only(t, "bgwriter_stats_reset"),
		"resetting bgwriter must move bgwriter's own clock")

	if server.major >= 17 {
		assert.Equal(t,
			blocks[0].only(t, "checkpointer_stats_reset"), blocks[1].only(t, "checkpointer_stats_reset"),
			"and must leave pg_stat_checkpointer's where it was: two views, two clocks, which is "+
				"why one column would leave the other view's counter with an undetectable reset")

		return
	}

	assert.NotEqual(t,
		blocks[0].only(t, "checkpointer_stats_reset"), blocks[1].only(t, "checkpointer_stats_reset"),
		"below 17 the reset takes the checkpoint counters with it, because they are one view")
	assert.Equal(t,
		blocks[1].only(t, "bgwriter_stats_reset"), blocks[1].only(t, "checkpointer_stats_reset"),
		"and one clock is read into both columns")
}

func matrixHoldSessions(t *testing.T, target Target, n int) (release func()) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	held := make([]*Conn, 0, n)

	for range n {
		conn, err := Connect(ctx, target)
		require.NoError(t, err)

		held = append(held, conn)
	}

	return func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		for _, conn := range held {
			_ = conn.Close(closeCtx)
		}
	}
}

func matrixExec(t *testing.T, target Target, sql string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	if !assert.NoError(t, err) {
		return
	}

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	rows, err := conn.Query(ctx, sql)
	if !assert.NoError(t, err, "%s", sql) {
		return
	}

	rows.Close()
	assert.NoError(t, rows.Err(), "%s", sql)
}

func matrixArtifactText(t *testing.T, result ArtifactResult) string {
	t.Helper()

	content, err := os.ReadFile(result.Artifact.FileName)
	require.NoError(t, err)
	result.File.Close()

	return string(content)
}

const matrixReplicationWindow = 2 * time.Second

const matrixWALSenderName = "yc-360-matrix-walsender"

const (
	matrixPlainSlot    = "yc_matrix_plain"
	matrixReservedSlot = "yc_matrix_reserved"
)

var matrixOptionalColumns = map[int]string{
	14: "",
	15: "",
	16: "conflicting",
	17: "conflicting,failover,inactive_since,invalidation_reason,synced",
	18: "conflicting,failover,inactive_since,invalidation_reason,synced,two_phase_at",
}

func matrixSuperuser(t *testing.T) matrixRole {
	t.Helper()

	for _, role := range matrixRoles {
		if role.superuser {
			return role
		}
	}

	t.Fatal("the matrix has no superuser role, and creating a WAL sender needs REPLICATION")

	return matrixRole{}
}

func matrixMonitor(t *testing.T) matrixRole {
	t.Helper()

	for _, role := range matrixRoles {
		if role.monitor {
			return role
		}
	}

	t.Fatal("the matrix has no pg_monitor role")

	return matrixRole{}
}

func matrixRestricted(t *testing.T) matrixRole {
	t.Helper()

	for _, role := range matrixRoles {
		if !role.privileged() {
			return role
		}
	}

	t.Fatal("the matrix has no floor role")

	return matrixRole{}
}

func matrixStartWALSender(t *testing.T, server matrixServer) (stop func()) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	config, err := pgconn.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s database=%s sslmode=%s",
		target.Host, target.Port, target.Username, target.Password,
		target.Database, target.SSLMode))
	require.NoError(t, err)

	config.RuntimeParams["replication"] = "database"
	config.RuntimeParams["application_name"] = matrixWALSenderName

	ctx, cancel := context.WithCancel(context.Background())

	conn, err := pgconn.ConnectConfig(ctx, config)
	if err != nil {
		cancel()
		t.Fatalf("open a replication connection to %s: %v", target, err)
	}

	identified, err := conn.Exec(ctx, "IDENTIFY_SYSTEM").ReadAll()
	require.NoError(t, err)
	require.NotEmpty(t, identified)
	require.NotEmpty(t, identified[0].Rows)

	lsn := string(identified[0].Rows[0][2])

	streaming := make(chan struct{})

	go func() {
		defer close(streaming)

		_, _ = conn.Exec(ctx, "START_REPLICATION "+lsn+" TIMELINE 1").ReadAll()
	}()

	stopped := false
	stop = func() {
		if stopped {
			return
		}
		stopped = true

		cancel()
		<-streaming

		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}

	require.Eventually(t, func() bool {
		return matrixWALSenderState(t, target) == "streaming"
	}, ModuleDeadline, 100*time.Millisecond,
		"the WAL sender never reached streaming, so the masking assertions would prove nothing")

	return stop
}

const matrixWALSenderStateSQL = `SELECT state FROM pg_catalog.pg_stat_replication
WHERE application_name = $1`

func matrixWALSenderState(t *testing.T, target Target) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), StatementTimeout)
	defer cancel()

	conn, err := Connect(ctx, target)
	if !assert.NoError(t, err) {
		return ""
	}

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	var state *string
	if err := conn.QueryRow(ctx, matrixWALSenderStateSQL, matrixWALSenderName).Scan(&state); err != nil {
		return ""
	}

	if state == nil {
		return ""
	}

	return *state
}

func matrixCreateSlot(t *testing.T, server matrixServer, name string, reserve bool) (drop func()) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	matrixExec(t, target, fmt.Sprintf(
		"SELECT pg_create_physical_replication_slot(%s, %t)", quoteLiteral(name), reserve))

	return func() {
		matrixExec(t, target, fmt.Sprintf(
			"SELECT pg_drop_replication_slot(%s)", quoteLiteral(name)))
	}
}

func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runMatrixReplicationWindow(t *testing.T, target Target) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration:   matrixReplicationWindow,
		Target:     target,
		Collectors: []Collector{Replication{Interval: time.Second}},
	}

	return window.Run(context.Background())
}

func (b capacityMatrixBlock) rowWhere(t *testing.T, column, value string) []string {
	t.Helper()

	at := b.index(t, column)

	for _, row := range b.rows {
		if row[at] == value {
			return row
		}
	}

	t.Fatalf("%s: no row with %s=%s", b.header["source"], column, value)

	return nil
}

func (b capacityMatrixBlock) cell(t *testing.T, row []string, column string) string {
	t.Helper()

	return row[b.index(t, column)]
}

func TestMatrixReplication(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			defer matrixCreateSlot(t, server, matrixPlainSlot, false)()
			defer matrixCreateSlot(t, server, matrixReservedSlot, true)()
			defer matrixStartWALSender(t, server)()

			assertMatrixViewColumns(t, server)

			for _, role := range matrixRoles {
				t.Run(role.user, func(t *testing.T) {
					target := matrixTarget(server, role)

					results := runMatrixReplicationWindow(t, target)
					require.Len(t, results, 1)
					require.NoError(t, results[0].IOErr)

					require.Equal(t, StatusComplete, results[0].Status,
						"neither view needs a grant: pg_replication_slots is open to a role "+
							"holding only LOGIN, and pg_stat_replication masks columns rather "+
							"than refusing the statement")
					require.Equal(t, 2, results[0].SamplesWritten)

					artifact := matrixArtifactText(t, results[0])
					assert.NotContains(t, artifact, target.Password,
						"the artifact carries the password")

					senders := parseCapacityBlocks(t, artifact, "pg_stat_replication")
					require.Len(t, senders, 2, "one senders block on every sample")

					slots := parseCapacityBlocks(t, artifact, "pg_replication_slots")
					require.Len(t, slots, 2)

					assertMatrixSenderColumns(t, senders[0])
					assertMatrixMasking(t, senders[0], role)

					assertMatrixOptionalColumns(t, slots[0], server)
					assertMatrixSlotsRoundTrip(t, slots[0])
				})
			}
		})
	}
}

var matrixSenderViewColumns = []string{
	"pid", "usesysid", "usename", "application_name", "client_addr",
	"client_hostname", "client_port", "backend_start", "backend_xmin", "state",
	"sent_lsn", "write_lsn", "flush_lsn", "replay_lsn",
	"write_lag", "flush_lag", "replay_lag",
	"sync_priority", "sync_state", "reply_time",
}

func matrixSlotViewColumns(server matrixServer) []string {
	optional := matrixOptionalColumns[server.major]
	if optional == "" {
		return stableSlotColumns
	}

	return append(slices.Clone(stableSlotColumns), strings.Split(optional, ",")...)
}

const matrixViewColumnsSQL = `SELECT attname::text FROM pg_catalog.pg_attribute
WHERE attrelid = to_regclass($1) AND attnum > 0 AND NOT attisdropped`

func matrixViewColumns(t *testing.T, target Target, view string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	rows, err := conn.Query(ctx, matrixViewColumnsSQL, view)
	require.NoError(t, err)

	defer rows.Close()

	var columns []string

	for rows.Next() {
		var name string

		require.NoError(t, rows.Scan(&name))

		columns = append(columns, name)
	}

	require.NoError(t, rows.Err())

	return columns
}

func assertMatrixViewColumns(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	assert.ElementsMatch(t, matrixSenderViewColumns,
		matrixViewColumns(t, target, "pg_catalog.pg_stat_replication"),
		"pg_stat_replication is no longer column-identical across the supported range, so the "+
			"collector needs Capacity's treatment: one capability flag, two statements")

	assert.ElementsMatch(t, matrixSlotViewColumns(server),
		matrixViewColumns(t, target, "pg_catalog.pg_replication_slots"),
		"pg_replication_slots gained or lost a column: a new one needs an optionalSlotColumns "+
			"entry, which is what keeps one statement covering every version")
}

func assertMatrixSenderColumns(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	assert.NotContains(t, block.rawHead, "error=",
		"the statement ran, which is what says every column it names exists on this version")
	assert.Equal(t, replicationColumns, block.columns)

	assert.Contains(t, block.rawHead, "source=pg_stat_replication ")
	assert.Contains(t, block.rawHead, "scope=cluster ")
}

func assertMatrixMasking(t *testing.T, block capacityMatrixBlock, role matrixRole) {
	t.Helper()

	row := block.rowWhere(t, "application_name", matrixWALSenderName)

	assert.NotEmpty(t, block.cell(t, row, "pid"))
	assert.NotEmpty(t, block.cell(t, row, "usesysid"))
	assert.NotEmpty(t, block.cell(t, row, "usename"))

	if role.privileged() {
		assert.Equal(t, "streaming", block.cell(t, row, "state"))
		assert.NotEmpty(t, block.cell(t, row, "sent_lsn"))
		assert.NotEmpty(t, block.cell(t, row, "client_addr"))
		assert.NotEmpty(t, block.cell(t, row, "backend_start"))
		assert.Equal(t, "async", block.cell(t, row, "sync_state"))

		return
	}

	for _, column := range replicationColumns[4:] {
		assert.Empty(t, block.cell(t, row, column),
			"%s is masked for a role without pg_monitor", column)
	}

	assert.Len(t, replicationColumns[4:], 16,
		"and there are sixteen of them, which is the measurement this assertion encodes")
}

func assertMatrixOptionalColumns(t *testing.T, block capacityMatrixBlock, server matrixServer) {
	t.Helper()

	assert.NotContains(t, block.rawHead, "error=",
		"pg_replication_slots needs no grant, and the to_jsonb extraction of a column the "+
			"server does not have yields NULL rather than raising")
	assert.Equal(t, slotColumns, block.columns,
		"21 columns on every version, which is the whole point of the union")

	want, known := matrixOptionalColumns[server.major]
	require.True(t, known, "pg%d has no expected optional set recorded", server.major)

	if want == "" {
		assert.NotContains(t, block.rawHead, "optional_columns",
			"absent rather than empty: string_agg over no matching rows is NULL, and a NULL "+
				"header value is never written")

		return
	}

	assert.Contains(t, block.rawHead, "optional_columns="+want+" ",
		"exactly the columns this version has - a new one in PostgreSQL 19 fails here rather "+
			"than passing silently")
}

func assertMatrixSlotsRoundTrip(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	plain := block.rowWhere(t, "slot_name", matrixPlainSlot)

	assert.Equal(t, "physical", block.cell(t, plain, "slot_type"))
	assert.Empty(t, block.cell(t, plain, "restart_lsn"),
		"a slot that never reserved WAL has no restart_lsn")
	assert.Empty(t, block.cell(t, plain, "wal_status"),
		"and a NULL wal_status beside it, which is the empty-versus-zero rule one level up: "+
			"this is not a lost slot, it is a slot that has reserved nothing")
	assert.Empty(t, block.cell(t, plain, "plugin"),
		"NULL for a physical slot by definition, never an empty plugin name")

	reserved := block.rowWhere(t, "slot_name", matrixReservedSlot)

	assert.Equal(t, "reserved", block.cell(t, reserved, "wal_status"))
	assert.NotEmpty(t, block.cell(t, reserved, "restart_lsn"),
		"immediately_reserve is what puts a value in both columns")

	for _, row := range [][]string{plain, reserved} {
		assert.Empty(t, block.cell(t, row, "safe_wal_size"),
			"max_slot_wal_keep_size is at its -1 default on every matrix container, and it is a "+
				"cluster GUC rather than a per-slot property - so this column is empty for "+
				"every slot, never mixed")
	}
}

const matrixSessionsWindow = 2 * time.Second

const matrixChainAppName = "yc-360-matrix-chain"

const (
	matrixChainRowSQL    = "SELECT id FROM yc_bloat_orders ORDER BY id LIMIT 1 FOR UPDATE"
	matrixAdvisoryFirst  = 1
	matrixAdvisorySecond = 2
)

func TestMatrixSessions(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			assertMatrixSessionViewColumns(t, server)

			chain := matrixHoldBlockingChain(t, server)
			defer chain.release()

			sessions := map[string]capacityMatrixBlock{}
			locks := map[string]capacityMatrixBlock{}

			for _, role := range matrixRoles {
				t.Run(role.user, func(t *testing.T) {
					target := matrixTarget(server, role)

					results := runMatrixSessionsWindow(t, target)
					require.Len(t, results, 1)
					require.NoError(t, results[0].IOErr)

					require.Equal(t, StatusComplete, results[0].Status,
						"neither view needs a grant: pg_locks is open to a role holding only "+
							"LOGIN, and pg_stat_activity masks columns rather than refusing "+
							"the statement or dropping the row")
					require.Equal(t, 2, results[0].SamplesWritten)

					artifact := matrixArtifactText(t, results[0])
					assert.NotContains(t, artifact, target.Password,
						"the artifact carries the password")

					activityBlocks := parseCapacityBlocks(t, artifact, "pg_stat_activity")
					require.Len(t, activityBlocks, 2, "one activity block on every sample")

					lockBlocks := parseCapacityBlocks(t, artifact, "pg_locks")
					require.Len(t, lockBlocks, 2, "and one locks block beside it")

					for _, block := range append(activityBlocks, lockBlocks...) {
						assert.NotContains(t, block.rawHead, "error=",
							"%s: the statement ran, which is what says every column it names "+
								"exists on this version", block.header["source"])
					}

					assert.Equal(t, sessionColumns, activityBlocks[0].columns)
					assert.Equal(t, lockColumns, lockBlocks[0].columns)

					assertMatrixSessionsCapturesItsOwnBackend(t, activityBlocks[0])

					sessions[role.user] = activityBlocks[0]
					locks[role.user] = lockBlocks[0]
				})
			}

			t.Run("the chain round-trips", func(t *testing.T) {
				assertMatrixChainRoundTrips(t, sessions, locks, chain)
			})

			t.Run("every cast column carries a live value", func(t *testing.T) {
				assertMatrixSessionsCastsScanLiveValues(t, sessions, locks)
			})

			t.Run("the privilege floor", func(t *testing.T) {
				assertMatrixSessionsMasking(t, sessions, chain)
				assertMatrixLocksNeedNoGrant(t, locks, chain)
			})

			t.Run("a timed-out statement costs the statement", func(t *testing.T) {
				assertMatrixSessionsTimeoutSparesTheConnection(t, server)
			})
		})
	}
}

func runMatrixSessionsWindow(t *testing.T, target Target) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration:   matrixSessionsWindow,
		Target:     target,
		Collectors: []Collector{Sessions{Interval: time.Second}},
	}

	return window.Run(context.Background())
}

func assertMatrixSessionViewColumns(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	assert.ElementsMatch(t, sessionColumns,
		matrixViewColumns(t, target, "pg_catalog.pg_stat_activity"),
		"pg_stat_activity is no longer column-identical across the supported range, or the "+
			"collector stopped taking every column of it")

	assert.ElementsMatch(t, lockColumns,
		matrixViewColumns(t, target, "pg_catalog.pg_locks"),
		"pg_locks is no longer column-identical across the supported range, or the collector "+
			"stopped taking every column of it")
}

type matrixChain struct {
	blockerPID string
	waiterPID  string

	release func()
}

func matrixHoldBlockingChain(t *testing.T, server matrixServer) matrixChain {
	t.Helper()

	blocker := matrixWritableConn(t, server)
	waiter := matrixWritableConn(t, server)

	matrixChainExec(t, blocker, "BEGIN")
	matrixChainExec(t, blocker, matrixChainRowSQL)
	matrixChainExec(t, blocker, fmt.Sprintf("SELECT pg_advisory_lock(%d), pg_advisory_lock(%d)",
		matrixAdvisoryFirst, matrixAdvisorySecond))

	blocked := make(chan struct{})

	go func() {
		defer close(blocked)

		_, _ = waiter.Exec(context.Background(), "BEGIN; "+matrixChainRowSQL).ReadAll()
	}()

	target := matrixTarget(server, matrixSuperuser(t))

	blockerPID := strconv.FormatUint(uint64(blocker.PID()), 10)
	waiterPID := strconv.FormatUint(uint64(waiter.PID()), 10)

	require.Eventually(t, func() bool {
		return matrixUngrantedLocks(t, target, waiterPID) > 0
	}, ModuleDeadline, 100*time.Millisecond,
		"the waiter never blocked, so every assertion below would prove nothing")

	release := func() {
		ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer cancel()

		_, _ = blocker.Exec(ctx, "ROLLBACK").ReadAll()
		<-blocked
		_, _ = waiter.Exec(ctx, "ROLLBACK").ReadAll()

		_ = blocker.Close(ctx)
		_ = waiter.Close(ctx)
	}

	return matrixChain{blockerPID: blockerPID, waiterPID: waiterPID, release: release}
}

func matrixWritableConn(t *testing.T, server matrixServer) *pgconn.PgConn {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	config, err := pgconn.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s database=%s sslmode=%s",
		target.Host, target.Port, target.Username, target.Password,
		target.Database, target.SSLMode))
	require.NoError(t, err)

	config.RuntimeParams["application_name"] = matrixChainAppName

	ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
	defer cancel()

	conn, err := pgconn.ConnectConfig(ctx, config)
	require.NoError(t, err, "open a writable connection to %s", target)

	return conn
}

func matrixChainExec(t *testing.T, conn *pgconn.PgConn, sql string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	_, err := conn.Exec(ctx, sql).ReadAll()
	require.NoError(t, err, "%s", sql)
}

func matrixUngrantedLocks(t *testing.T, target Target, pid string) int {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	var count int
	require.NoError(t, conn.QueryRow(ctx,
		"SELECT count(*)::int FROM pg_catalog.pg_locks WHERE pid = $1::int AND NOT granted",
		pid).Scan(&count))

	return count
}

func assertMatrixSessionsCapturesItsOwnBackend(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	row := block.rowWhere(t, "application_name", ApplicationName)

	assert.NotEmpty(t, block.cell(t, row, "pid"),
		"the statement has no WHERE clause, so the capture's own backend is in the block like "+
			"any other - and application_name survives masking, which is what makes this "+
			"comparison work at every privilege level")
}

func assertMatrixChainRoundTrips(t *testing.T, sessions, locks map[string]capacityMatrixBlock,
	chain matrixChain,
) {
	t.Helper()

	block, ok := locks["yc_monitor"]
	require.True(t, ok, "the monitor role's capture is missing")

	var waiting, holding []string

	for _, row := range block.rows {
		if block.cell(t, row, "locktype") != "transactionid" {
			continue
		}

		switch block.cell(t, row, "pid") {
		case chain.waiterPID:
			if block.cell(t, row, "granted") == "false" {
				waiting = row
			}

		case chain.blockerPID:
			if block.cell(t, row, "granted") == "true" {
				holding = row
			}
		}
	}

	require.NotNil(t, waiting, "no ungranted transactionid row for the blocked backend")
	require.NotNil(t, holding, "no granted transactionid row for the blocking backend")

	assert.Equal(t, block.cell(t, holding, "transactionid"),
		block.cell(t, waiting, "transactionid"),
		"the first hop: the ungranted row and the granted row for the same transaction id "+
			"name the blocker, with one equi-join and no pg_blocking_pids()")

	assert.NotEmpty(t, block.cell(t, waiting, "waitstart"),
		"and the wait's clock is on the ungranted row, so one sample carries its duration")
	assert.Empty(t, block.cell(t, holding, "waitstart"),
		"where a granted row has none")

	activity, ok := sessions["yc_monitor"]
	require.True(t, ok)

	blocked := activity.rowWhere(t, "pid", chain.waiterPID)
	assert.Equal(t, "Lock", activity.cell(t, blocked, "wait_event_type"),
		"and the other block agrees about the same backend, which is the cross-block join the "+
			"server performs within one sample")
}

func assertMatrixSessionsCastsScanLiveValues(t *testing.T, sessions, locks map[string]capacityMatrixBlock) {
	t.Helper()

	populated := func(block capacityMatrixBlock, column string) bool {
		at := block.index(t, column)

		for _, row := range block.rows {
			if at < len(row) && row[at] != "" {
				return true
			}
		}

		return false
	}

	activity, ok := sessions["yc_monitor"]
	require.True(t, ok)

	for _, column := range []string{
		"datid", "usesysid", "backend_xid", "backend_xmin", "client_addr", "client_port",
		"query_id",
	} {
		assert.True(t, populated(activity, column),
			"%s was NULL on every row, so this run proves nothing about its cast", column)
	}

	block, ok := locks["yc_monitor"]
	require.True(t, ok)

	for _, column := range []string{
		"database", "relation", "page", "tuple", "transactionid",
		"classid", "objid", "objsubid", "waitstart",
	} {
		assert.True(t, populated(block, column),
			"%s was NULL on every row, so this run proves nothing about its cast", column)
	}

	assertMatrixAdvisoryLocksAreTotallyOrdered(t, block)
}

func assertMatrixAdvisoryLocksAreTotallyOrdered(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	var advisory [][]string

	for _, row := range block.rows {
		if block.cell(t, row, "locktype") == "advisory" {
			advisory = append(advisory, row)
		}
	}

	require.Len(t, advisory, 2, "the fixture's two advisory locks")

	for _, column := range []string{"pid", "locktype", "relation", "page", "tuple", "mode"} {
		assert.Equal(t, block.cell(t, advisory[0], column), block.cell(t, advisory[1], column),
			"%s: the two rows tie on the first draft's sort key, which is why the ordering "+
				"carries the full lock identity - a nondeterministic cap boundary is exactly "+
				"what the ordering rule exists to prevent", column)
	}

	assert.Equal(t, strconv.Itoa(matrixAdvisoryFirst), block.cell(t, advisory[0], "objid"))
	assert.Equal(t, strconv.Itoa(matrixAdvisorySecond), block.cell(t, advisory[1], "objid"),
		"and objid is what separates them, in the order the statement asked for")
}

func assertMatrixSessionsMasking(t *testing.T, sessions map[string]capacityMatrixBlock,
	chain matrixChain,
) {
	t.Helper()

	monitor, ok := sessions["yc_monitor"]
	require.True(t, ok)

	restricted, ok := sessions["yc_restricted"]
	require.True(t, ok)

	for _, pid := range []string{chain.blockerPID, chain.waiterPID} {
		monitorRow := monitor.rowWhere(t, "pid", pid)
		restrictedRow := restricted.rowWhere(t, "pid", pid)

		for _, column := range []string{
			"pid", "datid", "datname", "usesysid", "usename", "application_name",
		} {
			assert.NotEmpty(t, restricted.cell(t, restrictedRow, column),
				"%s is the measured identity floor: a least-privilege capture still says "+
					"which database, which role and which application", column)
			assert.Equal(t, monitor.cell(t, monitorRow, column),
				restricted.cell(t, restrictedRow, column),
				"%s reads the same at both privilege levels", column)
		}

		for _, column := range []string{
			"backend_type", "state", "wait_event_type", "backend_start", "xact_start",
			"query_start", "state_change", "query_id", "client_addr", "client_port",
		} {
			assert.NotEmpty(t, monitor.cell(t, monitorRow, column),
				"%s: the monitor role reads it, which is what makes the emptiness below a "+
					"privilege result rather than an absent value", column)
			assert.Empty(t, restricted.cell(t, restrictedRow, column),
				"%s is masked without pg_read_all_stats", column)
		}

		query := restricted.cell(t, restrictedRow, "query")
		assert.NotEmpty(t, query,
			"query masks to a sentence rather than to NULL - the one column in the feature "+
				"where an empty cell is not what a denial looks like")
		assert.NotEqual(t, monitor.cell(t, monitorRow, "query"), query,
			"and it is not the statement the monitor role reads")
		t.Logf("masked query renders as %q", query)
	}

	blockerAsMonitor := monitor.rowWhere(t, "pid", chain.blockerPID)
	blockerAsRestricted := restricted.rowWhere(t, "pid", chain.blockerPID)

	assert.NotEmpty(t, restricted.cell(t, blockerAsRestricted, "backend_xid"))
	assert.Equal(t, monitor.cell(t, blockerAsMonitor, "backend_xid"),
		restricted.cell(t, blockerAsRestricted, "backend_xid"),
		"which is what lets a least-privilege capture still name the session holding the "+
			"transaction a chain is queued behind")

	assert.Empty(t, restricted.cell(t, restricted.rowWhere(t, "pid", chain.waiterPID), "backend_xid"))
	assert.Empty(t, monitor.cell(t, monitor.rowWhere(t, "pid", chain.waiterPID), "backend_xid"),
		"and the waiter has none to lose")
}

func assertMatrixLocksNeedNoGrant(t *testing.T, locks map[string]capacityMatrixBlock,
	chain matrixChain,
) {
	t.Helper()

	monitor, ok := locks["yc_monitor"]
	require.True(t, ok)

	restricted, ok := locks["yc_restricted"]
	require.True(t, ok)

	fixture := func(block capacityMatrixBlock) [][]string {
		var rows [][]string

		for _, row := range block.rows {
			switch block.cell(t, row, "pid") {
			case chain.blockerPID, chain.waiterPID:
				rows = append(rows, row)
			}
		}

		return rows
	}

	held := fixture(monitor)
	require.NotEmpty(t, held, "the chain holds no locks, so this proves nothing")

	assert.Equal(t, held, fixture(restricted),
		"a role holding nothing but LOGIN reads the same rows and the same columns a "+
			"pg_monitor member does - so at the privilege floor the capture still shapes the "+
			"chain while being unable to quote a single statement")
}

func assertMatrixSessionsTimeoutSparesTheConnection(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	setSessionsTimeout(ctx, conn)
	defer resetSessionsTimeout(ctx, conn)

	sleep := (SessionsStatementTimeout + time.Second).Seconds()

	rows, err := conn.Query(ctx, "SELECT pg_sleep($1)", sleep)
	if err == nil {
		for rows.Next() {
		}

		err = rows.Err()
		rows.Close()
	}

	require.Error(t, err, "pg_sleep(%.1f) outran the sample's own statement timeout", sleep)
	assert.True(t, hasSQLState(err, "57014"),
		"the server cancelled the statement and said so through the normal protocol: %v", err)

	var alive int
	require.NoError(t, conn.QueryRow(ctx, "SELECT 1").Scan(&alive),
		"the connection did not survive its own statement timeout, which is the failure the "+
			"server-side SET exists to prevent")
	assert.Equal(t, 1, alive)
}

const matrixSlowQueriesWindow = 2 * time.Second

const (
	matrixSecondDB     = "yc_second"
	matrixExtSchema    = "yc_ext"
	matrixAnchorMarker = "yc_360_matrix_anchor"
)

var matrixStatementOptionalColumns = map[int]string{
	14: "blk_read_time,blk_write_time,toplevel",
	15: "blk_read_time,blk_write_time,temp_blk_read_time,temp_blk_write_time,toplevel",
	16: "blk_read_time,blk_write_time,temp_blk_read_time,temp_blk_write_time,toplevel",
	17: "local_blk_read_time,local_blk_write_time,minmax_stats_since,shared_blk_read_time," +
		"shared_blk_write_time,stats_since,temp_blk_read_time,temp_blk_write_time,toplevel",
	18: "local_blk_read_time,local_blk_write_time,minmax_stats_since,shared_blk_read_time," +
		"shared_blk_write_time,stats_since,temp_blk_read_time,temp_blk_write_time,toplevel",
}

func matrixTargetDB(server matrixServer, role matrixRole, database string) Target {
	target := matrixTarget(server, role)
	target.Database = database

	return target
}

func matrixDDL(t *testing.T, server matrixServer, database string, statements ...string) {
	t.Helper()

	target := matrixTargetDB(server, matrixSuperuser(t), database)

	config, err := pgconn.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s database=%s sslmode=%s",
		target.Host, target.Port, target.Username, target.Password,
		target.Database, target.SSLMode))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := pgconn.ConnectConfig(ctx, config)
	require.NoError(t, err, "open a writable connection to %s", target)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	for _, sql := range statements {
		_, err := conn.Exec(ctx, sql).ReadAll()
		require.NoError(t, err, "%s", sql)
	}
}

func matrixQuery(t *testing.T, target Target, sql string, args ...any) []*string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	rows, err := conn.Query(ctx, sql, args...)
	require.NoError(t, err, "%s", sql)

	defer rows.Close()

	require.True(t, rows.Next(), "no row: %s", sql)

	values := make([]*string, len(rows.FieldDescriptions()))

	dest := make([]any, len(values))
	for i := range values {
		dest[i] = &values[i]
	}

	require.NoError(t, rows.Scan(dest...))
	rows.Close()
	require.NoError(t, rows.Err())

	return values
}

func runMatrixSlowQueriesWindow(t *testing.T, target Target, collector *SlowQueries) []ArtifactResult {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration:   matrixSlowQueriesWindow,
		Target:     target,
		Collectors: []Collector{collector},
	}

	return window.Run(context.Background())
}

func matrixSlowQueriesBlocks(t *testing.T, target Target,
	collector *SlowQueries,
) (statements, info []capacityMatrixBlock) {
	t.Helper()

	results := runMatrixSlowQueriesWindow(t, target, collector)
	require.Len(t, results, 1)
	require.NoError(t, results[0].IOErr)
	require.Equal(t, StatusComplete, results[0].Status)

	artifact := matrixArtifactText(t, results[0])

	for _, source := range []string{"pg_stat_statements", "pg_stat_statements_info"} {
		for _, block := range parseCapacityBlocks(t, artifact, source) {
			assert.NotContains(t, block.rawHead, target.Password,
				"%s: a block header carries the connection password", source)
		}
	}

	return parseCapacityBlocks(t, artifact, "pg_stat_statements"),
		parseCapacityBlocks(t, artifact, "pg_stat_statements_info")
}

func matrixAnchor(t *testing.T, server matrixServer) {
	t.Helper()

	matrixDDL(t, server, "postgres",
		"CREATE TABLE yc_360_matrix_anchor_tbl (n int)",
		"INSERT INTO yc_360_matrix_anchor_tbl SELECT g AS "+matrixAnchorMarker+
			" FROM generate_series(1, 11) g",
		"SELECT count(*) AS "+matrixAnchorMarker+" FROM yc_360_matrix_anchor_tbl",
		"DROP TABLE yc_360_matrix_anchor_tbl",
	)
}

func TestMatrixSlowQueries(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			assertMatrixStatementViewColumns(t, server)

			matrixAnchor(t, server)

			statements := map[string]capacityMatrixBlock{}
			infos := map[string]capacityMatrixBlock{}
			closing := map[string]capacityMatrixBlock{}

			for _, role := range matrixRoles {
				t.Run(role.user, func(t *testing.T) {
					target := matrixTarget(server, role)

					statementBlocks, infoBlocks := matrixSlowQueriesBlocks(t, target, NewSlowQueries())

					require.Len(t, statementBlocks, 2, "one statements block on every sample")
					require.Len(t, infoBlocks, 2, "and one info block beside it")

					for _, block := range append(statementBlocks, infoBlocks...) {
						assert.NotContains(t, block.rawHead, "error=",
							"%s: the statement ran, which is what says every column it names "+
								"exists on this extension version", block.header["source"])
						assert.NotContains(t, block.rawHead, "reason=",
							"%s: the extension is installed and reachable here",
							block.header["source"])
					}

					assert.Equal(t, statementColumns, statementBlocks[0].columns,
						"the same 37 columns on every extension version, which is what one "+
							"statement covering 1.8 through 1.12 buys")
					assert.Equal(t, infoColumns, infoBlocks[0].columns)

					assert.Equal(t, matrixStatementOptionalColumns[server.major],
						statementBlocks[0].header["optional_columns"],
						"the header is not lying about which columns the server had")

					assertMatrixInfoReadsAtTheFloor(t, infoBlocks)

					statements[role.user] = statementBlocks[0]
					infos[role.user] = infoBlocks[0]
					closing[role.user] = statementBlocks[1]
				})
			}

			t.Run("the privilege floor", func(t *testing.T) {
				assertMatrixStatementMasking(t, statements)
			})

			t.Run("every cell round-trips", func(t *testing.T) {
				assertMatrixStatementCellsRoundTrip(t, server, statements[matrixSuperuser(t).user])
			})

			t.Run("every cast column carries a live value", func(t *testing.T) {
				assertMatrixStatementCasts(t, statements[matrixSuperuser(t).user])
			})

			t.Run("the agent's own read appears in the closing sample", func(t *testing.T) {
				assertMatrixCaptureIsInItsOwnArtifact(t, closing[matrixSuperuser(t).user])
			})

			t.Run("the cap keeps a prefix of the key space", func(t *testing.T) {
				assertMatrixStatementCap(t, server)
			})

			t.Run("the settings hold at the privilege floor", func(t *testing.T) {
				assertMatrixStatementSettings(t, server)
			})

			t.Run("the extension is installed per database", func(t *testing.T) {
				assertMatrixExtensionIsPerDatabase(t, server)
			})

			t.Run("the floor, both sides", func(t *testing.T) {
				assertMatrixExtensionMinVersion(t, server)
			})

			t.Run("an extension outside search_path", func(t *testing.T) {
				assertMatrixExtensionNotInSearchPath(t, server)
			})

			t.Run("resets", func(t *testing.T) {
				assertMatrixStatementResets(t, server)
			})
		})
	}
}

func assertMatrixStatementViewColumns(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))
	columns := matrixViewColumns(t, target, "pg_stat_statements")

	require.NotEmpty(t, columns, "the extension is not installed in the entrypoint database")

	has := func(name string) bool { return slices.Contains(columns, name) }

	pre111 := server.major <= 16

	assert.Equal(t, pre111, has("blk_read_time"), "pg%d: blk_read_time", server.major)
	assert.Equal(t, pre111, has("blk_write_time"), "pg%d: blk_write_time", server.major)
	assert.Equal(t, !pre111, has("shared_blk_read_time"), "pg%d: shared_blk_read_time", server.major)
	assert.Equal(t, !pre111, has("shared_blk_write_time"), "pg%d: shared_blk_write_time", server.major)
	assert.Equal(t, !pre111, has("local_blk_read_time"), "pg%d: local_blk_read_time", server.major)
	assert.Equal(t, !pre111, has("stats_since"), "pg%d: stats_since", server.major)
	assert.Equal(t, !pre111, has("minmax_stats_since"), "pg%d: minmax_stats_since", server.major)

	assert.Equal(t, server.major >= 15, has("temp_blk_read_time"), "pg%d: temp_blk_read_time", server.major)

	assert.True(t, has("toplevel"), "pg%d: toplevel", server.major)

	assert.True(t, has("total_exec_time"), "pg%d: total_exec_time", server.major)

	present := []string{}

	for _, name := range optionalStatementColumns {
		if has(name) {
			present = append(present, name)
		}
	}

	sort.Strings(present)

	assert.Equal(t, matrixStatementOptionalColumns[server.major], strings.Join(present, ","),
		"pg%d: the recorded expectation and the catalogue disagree", server.major)
}

func assertMatrixInfoReadsAtTheFloor(t *testing.T, blocks []capacityMatrixBlock) {
	t.Helper()

	for _, block := range blocks {
		require.Len(t, block.rows, 1, "one row, two columns, no cap and no ordering")
		assert.NotEmpty(t, block.only(t, "dealloc"),
			"dealloc is readable by a role holding only LOGIN")
	}

	assert.Equal(t, blocks[0].only(t, "stats_reset"), blocks[1].only(t, "stats_reset"),
		"nothing reset the counters inside this window, so every delta in the file is a delta")
}

func assertMatrixStatementMasking(t *testing.T, blocks map[string]capacityMatrixBlock) {
	t.Helper()

	privileged := blocks["yc_monitor"]
	restricted := blocks["yc_restricted"]

	var anchor []string

	for _, row := range privileged.rows {
		if strings.Contains(privileged.cell(t, row, "query"), matrixAnchorMarker) {
			anchor = row
			break
		}
	}

	require.NotNil(t, anchor, "the anchor statement is not in the privileged capture")

	var (
		queryid   = privileged.cell(t, anchor, "queryid")
		userid    = privileged.cell(t, anchor, "userid")
		calls     = privileged.cell(t, anchor, "calls")
		execTime  = privileged.cell(t, anchor, "total_exec_time")
		anchorSQL = privileged.cell(t, anchor, "query")
	)

	assert.NotEmpty(t, queryid, "a role with pg_read_all_stats reads the key")

	var masked []string

	for _, row := range restricted.rows {
		if restricted.cell(t, row, "userid") == userid &&
			restricted.cell(t, row, "calls") == calls &&
			restricted.cell(t, row, "total_exec_time") == execTime {
			masked = row
			break
		}
	}

	require.NotNil(t, masked,
		"the row is returned to every role - masking takes columns, never rows")

	assert.Empty(t, restricted.cell(t, masked, "queryid"),
		"and the column it takes is the key, which is what makes this artifact's "+
			"least-privilege capture unmergeable")

	maskedSQL := restricted.cell(t, masked, "query")

	assert.NotEmpty(t, maskedSQL,
		"the server substitutes a sentence rather than an absence")
	assert.NotEqual(t, anchorSQL, maskedSQL,
		"asserted by inequality rather than against the sentinel's wording, so a future "+
			"rewording fails informatively instead of pinning the agent to a message it "+
			"never reads")

	assert.Equal(t, calls, restricted.cell(t, masked, "calls"),
		"and every counter is exact - which is why the report rule here is a bucket rather "+
			"than a suppression: these numbers are real and unattributable")
	assert.Equal(t, execTime, restricted.cell(t, masked, "total_exec_time"))

	var keyed int

	for _, row := range restricted.rows {
		if restricted.cell(t, row, "queryid") != "" {
			keyed++
		}
	}

	assert.NotZero(t, keyed, "the capture's own reads are the role's own statements")
	assert.Less(t, keyed, len(restricted.rows), "and most of the block is not")

	for i, row := range restricted.rows {
		if restricted.cell(t, row, "queryid") == "" {
			assert.GreaterOrEqual(t, i, keyed,
				"ASC sorts NULLs last, so a cap that binds at this privilege level sheds "+
					"the unattributable rows first")
		}
	}
}

func assertMatrixStatementCellsRoundTrip(t *testing.T, server matrixServer, block capacityMatrixBlock) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))
	present := matrixViewColumns(t, target, "pg_stat_statements")

	var anchor []string

	for _, row := range block.rows {
		if strings.Contains(block.cell(t, row, "query"), matrixAnchorMarker) {
			anchor = row
			break
		}
	}

	require.NotNil(t, anchor, "the anchor statement is not in the capture")

	var (
		selects []string
		names   []string
	)

	for _, spec := range statementColumnSpecs {
		if !slices.Contains(present, spec.name) {
			assert.Empty(t, block.cell(t, anchor, spec.name),
				"%s: absent from this extension version, so the cell is empty rather than zero",
				spec.name)

			continue
		}

		names = append(names, spec.name)

		switch spec.name {
		case "stats_since", "minmax_stats_since":
			selects = append(selects, fmt.Sprintf(
				`to_char(%s AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')`, spec.name))

		case "query":
			selects = append(selects, `replace(replace(query, chr(13), ' '), chr(10), ' ')`)

		default:
			selects = append(selects, spec.name+"::text")
		}
	}

	sql := "SELECT " + strings.Join(selects, ", ") + ` FROM pg_stat_statements
WHERE queryid = $1::bigint AND userid = $2::oid AND dbid = $3::oid`

	values := matrixQuery(t, target, sql,
		block.cell(t, anchor, "queryid"),
		block.cell(t, anchor, "userid"),
		block.cell(t, anchor, "dbid"))

	require.Len(t, values, len(names))

	for i, name := range names {
		want := ""
		if values[i] != nil {
			want = *values[i]
		}

		assertMatrixSameCell(t, name, want, block.cell(t, anchor, name))
	}
}

func assertMatrixSameCell(t *testing.T, name, want, got string) {
	t.Helper()

	if want == got {
		return
	}

	wantFloat, wantErr := strconv.ParseFloat(want, 64)
	gotFloat, gotErr := strconv.ParseFloat(got, 64)

	if wantErr == nil && gotErr == nil && wantFloat == gotFloat {
		return
	}

	assert.Failf(t, "cell does not round-trip",
		"%s: the artifact says %q where an independent read of the same entry says %q",
		name, got, want)
}

func assertMatrixStatementCasts(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	var walWritten int

	for _, row := range block.rows {
		assert.NotEmpty(t, block.cell(t, row, "userid"), "oid, cast to text")
		assert.NotEmpty(t, block.cell(t, row, "dbid"), "oid, cast to text")

		wal := block.cell(t, row, "wal_bytes")
		require.NotEmpty(t, wal, "numeric, cast to text")

		if wal != "0" {
			walWritten++
		}
	}

	assert.NotZero(t, walWritten,
		"wal_bytes is the one cast in this package taken on principle rather than after a "+
			"measured scan failure - numeric is unbounded where int64 is not - so a live "+
			"non-zero value is what says the cast is doing its job rather than sitting unused")
}

func assertMatrixCaptureIsInItsOwnArtifact(t *testing.T, block capacityMatrixBlock) {
	t.Helper()

	for _, row := range block.rows {
		if strings.HasPrefix(block.cell(t, row, "query"), "WITH m AS MATERIALIZED") {
			assert.NotEmpty(t, block.cell(t, row, "queryid"),
				"a role always sees the statements it executed itself")

			return
		}
	}

	t.Fatal("the opening read is not in the closing sample: this view has no application_name, " +
		"so the agent's own rows are identified by their statement text and the capture role's userid")
}

func assertMatrixStatementCap(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	blocks, _ := matrixSlowQueriesBlocks(t, target, &SlowQueries{MaxStatements: 5})
	block := blocks[0]

	assert.Equal(t, "5", block.header["statements_written"])
	assert.Equal(t, "true", block.header["truncated"])

	total, err := strconv.ParseInt(block.header["statements_total"], 10, 64)
	require.NoError(t, err)
	assert.Greater(t, total, int64(5),
		"count(*) OVER () is inside the CTE, so it is the uncapped total")

	require.Len(t, block.rows, 5)

	keys := make([]int64, 0, len(block.rows))

	for _, row := range block.rows {
		queryid := block.cell(t, row, "queryid")
		require.NotEmpty(t, queryid, "the superuser reads every key")

		key, err := strconv.ParseInt(queryid, 10, 64)
		require.NoError(t, err)

		keys = append(keys, key)
	}

	assert.True(t, slices.IsSorted(keys),
		"ordered on identity and never on a statistic: a top-N taken independently at each "+
			"endpoint selects two different sets and leaves a query with no baseline")

	smallest := matrixQuery(t, target,
		`SELECT min(queryid)::text FROM pg_stat_statements WHERE queryid IS NOT NULL`)
	require.NotNil(t, smallest[0])

	assert.Equal(t, *smallest[0], strconv.FormatInt(keys[0], 10),
		"the prefix starts where the key space does")
}

func assertMatrixStatementSettings(t *testing.T, server matrixServer) {
	t.Helper()

	var restricted matrixRole

	for _, role := range matrixRoles {
		if !role.privileged() {
			restricted = role
		}
	}

	m := collectFromMatrix(t, matrixTarget(server, restricted))

	assert.Equal(t, "off", m.TrackIOTiming)
	assert.Equal(t, "5000", m.PgStatStatementsMax)
	assert.Equal(t, "top", m.PgStatStatementsTrack,
		"the default, which is why toplevel is true on every row of these containers")
	assert.Equal(t, "off", m.PgStatStatementsTrackPlanning,
		"the default, which is why plans and the three plan-time columns are structurally zero")
	assert.Equal(t, "on", m.PgStatStatementsTrackUtility)

	for _, name := range []string{
		"track_io_timing",
		"pg_stat_statements.max",
		"pg_stat_statements.track",
		"pg_stat_statements.track_planning",
		"pg_stat_statements.track_utility",
	} {
		assert.NotContains(t, splitSettingList(m.SettingsUnavailable), name,
			"%s reads at the privilege floor, so it carries no superuser-only caveat", name)
	}
}

func assertMatrixExtensionIsPerDatabase(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

	statements, info := matrixSlowQueriesBlocks(t, target, NewSlowQueries())

	require.Len(t, statements, 2)
	require.Len(t, info, 2)

	for _, block := range append(statements, info...) {
		assert.Equal(t, reasonExtensionAbsent, block.header["reason"],
			"%s: one cause reads as one cause", block.header["source"])
		assert.NotContains(t, block.rawHead, "error=",
			"%s: an extension nobody created is not a read that failed", block.header["source"])
		assert.Empty(t, block.rows, "%s: header-only", block.header["source"])
	}

	assert.Equal(t, "true", statements[0].header["library_loaded"],
		"the second half of the diagnosis: this cluster's problem is CREATE EXTENSION and "+
			"not shared_preload_libraries")

	assert.Equal(t, statementColumns, statements[0].columns,
		"the column contract is written even with no rows, which is what keeps the file "+
			"non-empty and out of the upload path's zero-byte drop")
}

func assertMatrixExtensionMinVersion(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

	defer matrixDDL(t, server, matrixSecondDB, "DROP EXTENSION IF EXISTS pg_stat_statements")

	matrixDDL(t, server, matrixSecondDB, "CREATE EXTENSION pg_stat_statements VERSION '1.7'")

	statements, info := matrixSlowQueriesBlocks(t, target, NewSlowQueries())

	for _, block := range append(statements, info...) {
		assert.Equal(t, reasonExtensionTooOld, block.header["reason"], block.header["source"])
		assert.Equal(t, "1.7", block.header["extension_version"],
			"%s: named, so the operator knows which of ALTER EXTENSION ... UPDATE's "+
				"preconditions they are on", block.header["source"])
		assert.NotContains(t, block.rawHead, "error=", block.header["source"])
		assert.Empty(t, block.rows, block.header["source"])
	}

	matrixDDL(t, server, matrixSecondDB,
		"DROP EXTENSION pg_stat_statements",
		"CREATE EXTENSION pg_stat_statements VERSION '1.8'")

	statements, info = matrixSlowQueriesBlocks(t, target, NewSlowQueries())

	assert.NotContains(t, statements[0].rawHead, "reason=",
		"1.8 is a supported extension, not a refused one")
	assert.NotContains(t, statements[0].rawHead, "error=")
	assert.Equal(t, "1.8", statements[0].header["extension_version"])
	assert.Equal(t, "blk_read_time,blk_write_time", statements[0].header["optional_columns"],
		"the pre-rename pair is what 1.8 has of the eleven, and toplevel is not among them")
	assert.NotEmpty(t, statements[0].rows, "the floor's accept side returns rows")

	for _, row := range statements[0].rows {
		assert.Empty(t, statements[0].cell(t, row, "toplevel"),
			"toplevel arrives at 1.9, so the fourth component of the merge key is a NULL "+
				"the receiver has to treat as a key value")
		assert.NotEmpty(t, statements[0].cell(t, row, "total_exec_time"),
			"which is the column the floor is drawn at")
	}

	assert.Equal(t, reasonViewAbsent, info[0].header["reason"],
		"and it is the only state where the two blocks of a sample carry different reasons")
	assert.NotContains(t, info[0].rawHead, "error=")
}

func assertMatrixExtensionNotInSearchPath(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

	defer matrixDDL(t, server, matrixSecondDB,
		"DROP EXTENSION IF EXISTS pg_stat_statements",
		"DROP SCHEMA IF EXISTS "+matrixExtSchema)

	matrixDDL(t, server, matrixSecondDB,
		"CREATE SCHEMA "+matrixExtSchema,
		"CREATE EXTENSION pg_stat_statements SCHEMA "+matrixExtSchema)

	statements, info := matrixSlowQueriesBlocks(t, target, NewSlowQueries())

	for _, block := range append(statements, info...) {
		assert.Equal(t, reasonNotInSearchPath, block.header["reason"], block.header["source"])
		assert.NotContains(t, block.rawHead, "error=", block.header["source"])
	}

	assert.Equal(t, matrixExtSchema, statements[0].header["extension_schema"],
		"named, because the remedy is a search_path the operator has to write")
	assert.Equal(t, "true", statements[0].header["schema_usage"],
		"the superuser has USAGE, so this is a path problem and not a grant one - which is "+
			"the whole reason the key is here")
}

func assertMatrixStatementResets(t *testing.T, server matrixServer) {
	t.Helper()

	target := matrixTarget(server, matrixSuperuser(t))

	before := matrixQuery(t, target, `SELECT stats_reset::text, dealloc::text FROM pg_stat_statements_info`)
	require.NotNil(t, before[0])

	entry := matrixQuery(t, target,
		`SELECT userid::text, dbid::text, queryid::text FROM pg_stat_statements
		  WHERE queryid IS NOT NULL AND query LIKE '%'||$1||'%' LIMIT 1`, matrixAnchorMarker)
	require.NotNil(t, entry[2], "the anchor entry is gone before the reset item ran")

	matrixDDL(t, server, "postgres", fmt.Sprintf(
		"SELECT pg_stat_statements_reset(%s, %s, %s)", *entry[0], *entry[1], *entry[2]))

	after := matrixQuery(t, target, `SELECT stats_reset::text, dealloc::text FROM pg_stat_statements_info`)

	assert.Equal(t, *before[0], *after[0],
		"a targeted reset moves stats_reset not at all - pinned live so a future version "+
			"that changes the semantics is noticed rather than silently trusted")
	assert.Equal(t, *before[1], *after[1], "and deallocates nothing")

	matrixDDL(t, server, "postgres", "SELECT pg_stat_statements_reset()")

	full := matrixQuery(t, target, `SELECT stats_reset::text FROM pg_stat_statements_info`)

	assert.NotEqual(t, *before[0], *full[0],
		"which is the only in-band signal that every delta in a file spans a counter reset")
}

func matrixLogDir(server matrixServer) string {
	return fmt.Sprintf("/tmp/yc-pglogs/pg%d", server.major)
}

func requireMatrixLogDir(t *testing.T, server matrixServer) {
	t.Helper()

	if _, err := os.Stat(matrixLogDir(server)); err != nil {
		t.Skipf("the log bind mount is missing: run\n\n"+
			"  mkdir -p /tmp/yc-pglogs/pg{14,15,16,17,18} && chmod 777 /tmp/yc-pglogs/pg*\n"+
			"  docker compose -f compose.pg.yaml down -v && docker compose -f compose.pg.yaml up -d --wait\n\n%v", err)
	}
}

type matrixTail struct {
	t    *testing.T
	conn *Conn

	collector interface {
		Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
		WriteClosing(w io.Writer, s SampleContext) error
	}

	index int
}

func newMatrixTail(t *testing.T, target Target, collector interface {
	Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error
	WriteClosing(w io.Writer, s SampleContext) error
},
) *matrixTail {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err, "connect to %s", target)

	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	})

	return &matrixTail{t: t, conn: conn, collector: collector}
}

func (m *matrixTail) sample() textBlock {
	m.t.Helper()

	m.index++

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	var buf bytes.Buffer
	require.NoError(m.t, m.collector.Sample(ctx, m.conn, &buf, SampleContext{
		At:       time.Now(),
		Index:    m.index,
		Total:    12,
		Database: "postgres",
		redact:   func(err error) string { return errorText(err, "") },
	}))

	blocks := parseTextArtifact(m.t, buf.String())
	require.Len(m.t, blocks, 1)

	return blocks[0]
}

func (m *matrixTail) drain() []textBlock {
	m.t.Helper()

	var buf bytes.Buffer
	require.NoError(m.t, m.collector.WriteClosing(&buf, SampleContext{At: time.Now(), Database: "postgres"}))

	return parseTextArtifact(m.t, buf.String())
}

//nolint:unparam // yc_second is where the fixtures live today, not a property of the helper
func matrixLogConn(t *testing.T, server matrixServer, database string) *pgconn.PgConn {
	t.Helper()

	target := matrixTargetDB(server, matrixSuperuser(t), database)

	config, err := pgconn.ParseConfig(fmt.Sprintf(
		"host=%s port=%d user=%s password=%s database=%s sslmode=%s",
		target.Host, target.Port, target.Username, target.Password,
		target.Database, target.SSLMode))
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := pgconn.ConnectConfig(ctx, config)
	require.NoError(t, err)

	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	})

	return conn
}

func matrixLogExec(t *testing.T, conn *pgconn.PgConn, sql string) error {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	_, err := conn.Exec(ctx, sql).ReadAll()

	return err
}

func matrixDeadlockTable(t *testing.T, server matrixServer) {
	t.Helper()

	matrixDDL(t, server, "yc_second",
		"CREATE TABLE yc_dl (id int PRIMARY KEY, v int)",
		"INSERT INTO yc_dl VALUES (1, 1), (2, 2), (3, 3)")

	t.Cleanup(func() { matrixDDL(t, server, "yc_second", "DROP TABLE IF EXISTS yc_dl") })
}

func matrixGenerateDeadlock(t *testing.T, server matrixServer) {
	t.Helper()

	a := matrixLogConn(t, server, "yc_second")
	b := matrixLogConn(t, server, "yc_second")

	require.NoError(t, matrixLogExec(t, a, "BEGIN; UPDATE yc_dl SET v = v + 1 WHERE id = 1"))
	require.NoError(t, matrixLogExec(t, b, "BEGIN; UPDATE yc_dl SET v = v + 1 WHERE id = 2"))

	var (
		wg            sync.WaitGroup
		errA, errB    error
		crossedFirst  = "UPDATE yc_dl SET v = v + 1 WHERE id = 2"
		crossedSecond = "UPDATE yc_dl SET v = v + 1 WHERE id = 1"
	)

	wg.Add(2)

	go func() { defer wg.Done(); errA = matrixLogExec(t, a, crossedFirst) }()
	go func() { defer wg.Done(); errB = matrixLogExec(t, b, crossedSecond) }()

	wg.Wait()

	require.False(t, errA == nil && errB == nil, "one of the two crossed updates must be the victim")

	_ = matrixLogExec(t, a, "ROLLBACK")
	_ = matrixLogExec(t, b, "ROLLBACK")
}

func TestMatrixLogTailResolution(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		for _, role := range matrixRoles {
			t.Run(fmt.Sprintf("pg%d/%s", server.major, role.user), func(t *testing.T) {
				target := matrixTarget(server, role)

				block := newMatrixTail(t, target, NewDeadlocks()).sample()

				_, err := matrixCurrentLogfile(t, target)
				if matrixFunctionAllowed(server, role) {
					assert.NoError(t, err, "pg_current_logfile() is granted here")
				} else {
					require.Error(t, err, "pg_current_logfile() is denied to %s on pg%d",
						role.user, server.major)
					assert.Contains(t, err.Error(), "permission denied for function pg_current_logfile")
				}

				if !role.privileged() {
					assert.Equal(t, reasonUnresolved, block.fields["reason"],
						"the privilege floor has no route at all")
					assert.False(t, block.has("matched"))

					return
				}

				assert.Equal(t, LogAccessDirect, block.fields["log_access"],
					"Mode H resolves on 14 through 18 alike - which it did not before this slice")

				want := resolvedByGlob
				if matrixFunctionAllowed(server, role) {
					want = resolvedByFunction
				}

				assert.Equal(t, want, block.fields["log_resolved_by"],
					"route 1 cannot fire from the host: current_logfiles is in the container's "+
						"private data directory")

				assert.Equal(t, "stderr", block.fields["log_format"])
				assert.Equal(t, matchedByMessage, block.fields["matched_by"])
				assert.Equal(t, "0", block.fields["matched"], "sample 1 seeks to EOF")
				assert.True(t, strings.HasPrefix(block.fields["log_path"], matrixLogDir(server)))
			})
		}
	}
}

func matrixCurrentLogfile(t *testing.T, target Target) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err)

	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	}()

	var logfile *string
	if err := conn.QueryRow(ctx, logLocationSQL).Scan(&logfile); err != nil {
		return "", err
	}

	return text(logfile), nil
}

func TestMatrixLogTailDeadlock(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixDeadlockTable(t, server)

			tail := newMatrixTail(t, matrixTarget(server, matrixMonitor(t)), NewDeadlocks())
			require.Equal(t, LogAccessDirect, tail.sample().fields["log_access"])

			matrixGenerateDeadlock(t, server)

			block := matrixTailUntilMatched(t, server, tail)

			assert.Equal(t, "1", block.fields["matched"])
			assert.Contains(t, block.body, "deadlock detected")
			assert.Equal(t, 2, strings.Count(block.body, " waits for ShareLock"),
				"both participants of the wait cycle")
			assert.Equal(t, 4, strings.Count(block.body, "Process "),
				"the DETAIL is four lines - two waits and two statements - and three of them "+
					"are TAB continuations a line-oriented reader would mis-attribute")
			assert.Contains(t, block.body, "CONTEXT:",
				"and the line a DETAIL-only rule stops before, which names the relation and the tuple")
			assert.Contains(t, block.body, "STATEMENT:")

			size, err := strconv.Atoi(block.fields["bytes"])
			require.NoError(t, err)
			assert.Equal(t, size, len(block.body))
		})
	}
}

func matrixLogMarker(t *testing.T, server matrixServer) {
	t.Helper()

	matrixDDL(t, server, "postgres",
		"DO $$ BEGIN RAISE WARNING 'yc-360 log tail marker'; END $$")
}

func matrixTailUntilMatched(t *testing.T, server matrixServer, tail *matrixTail) textBlock {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for {
		matrixLogMarker(t, server)

		block := tail.sample()
		if block.fields["matched"] != "0" && block.fields["matched"] != "" {
			return block
		}

		if time.Now().After(deadline) {
			t.Fatalf("no event reached the log within the deadline: %s", block.header)
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func TestMatrixLogTailTimeouts(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixDeadlockTable(t, server)

			tail := newMatrixTail(t, matrixTarget(server, matrixMonitor(t)), NewTimeouts())
			require.Equal(t, LogAccessDirect, tail.sample().fields["log_access"])

			worker := matrixLogConn(t, server, "yc_second")
			require.Error(t, matrixLogExec(t, worker,
				"SET statement_timeout = '300ms'; SELECT pg_sleep(2)"))
			require.NoError(t, matrixLogExec(t, worker, "RESET statement_timeout"))

			holder := matrixLogConn(t, server, "yc_second")
			require.NoError(t, matrixLogExec(t, holder, "BEGIN; UPDATE yc_dl SET v = v + 1 WHERE id = 3"))

			require.Error(t, matrixLogExec(t, worker,
				"SET lock_timeout = '300ms'; UPDATE yc_dl SET v = v + 1 WHERE id = 3"))
			require.NoError(t, matrixLogExec(t, holder, "ROLLBACK"))

			idle := matrixLogConn(t, server, "yc_second")
			require.NoError(t, matrixLogExec(t, idle,
				"SET idle_in_transaction_session_timeout = '400ms'; BEGIN; SELECT 1"))
			time.Sleep(1500 * time.Millisecond)

			matrixLogMarker(t, server)

			body := matrixDrainBodies(t, tail)

			assert.Contains(t, body, "canceling statement due to statement timeout")
			assert.Contains(t, body, "canceling statement due to lock timeout")
			assert.Contains(t, body, "terminating connection due to idle-in-transaction timeout")

			assert.Contains(t, body, "CONTEXT:  while updating tuple",
				"the lock timeout's third line")

			idleLine := matrixEventStartingWith(t, body, "terminating connection due to idle-in-transaction timeout")
			assert.Equal(t, 1, strings.Count(idleLine, "\n"),
				"the idle-in-transaction FATAL is one line and cannot have a STATEMENT: line - "+
					"the timeout fires precisely because the backend is running no statement")
			assert.NotContains(t, idleLine, "STATEMENT:")
		})
	}
}

func matrixDrainBodies(t *testing.T, tail *matrixTail) string {
	t.Helper()

	var body strings.Builder

	for range 6 {
		body.WriteString(tail.sample().body)
		time.Sleep(500 * time.Millisecond)
	}

	for _, block := range tail.drain() {
		body.WriteString(block.body)
	}

	return body.String()
}

func matrixEventStartingWith(t *testing.T, body, message string) string {
	t.Helper()

	at := strings.Index(body, message)
	require.GreaterOrEqual(t, at, 0, "no event carrying %q in:\n%s", message, body)

	start := strings.LastIndexByte(body[:at], '\n') + 1

	rest := body[start:]
	if next := strings.Index(rest[1:], "\n20"); next >= 0 {
		rest = rest[:next+2]
	}

	return rest
}

func TestMatrixLogTailBoundaryUnderLoad(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixDeadlockTable(t, server)

			tail := newMatrixTail(t, matrixTarget(server, matrixMonitor(t)), NewDeadlocks())
			require.Equal(t, LogAccessDirect, tail.sample().fields["log_access"])

			noise := matrixLogConn(t, server, "yc_second")

			stop := make(chan struct{})
			var wg sync.WaitGroup

			wg.Add(1)
			go func() {
				defer wg.Done()

				for {
					select {
					case <-stop:
						return
					default:
						_ = matrixLogExec(t, noise, "SELECT 1/0")
						time.Sleep(20 * time.Millisecond)
					}
				}
			}()

			matrixGenerateDeadlock(t, server)

			close(stop)
			wg.Wait()

			matrixLogMarker(t, server)

			var body strings.Builder
			for range 6 {
				body.WriteString(tail.sample().body)
				time.Sleep(300 * time.Millisecond)
			}

			copied := body.String()

			assert.Equal(t, 1, strings.Count(copied, "deadlock detected"),
				"exactly one event in the copied bytes, whatever else the server was logging")
			assert.NotContains(t, copied, "division by zero",
				"and the unrelated ERRORs around it are not swept in")
		})
	}
}

func TestMatrixLogTailRotation(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixDeadlockTable(t, server)

			tail := newMatrixTail(t, matrixTarget(server, matrixSuperuser(t)), NewDeadlocks())
			require.Equal(t, LogAccessDirect, tail.sample().fields["log_access"])

			matrixGenerateDeadlock(t, server)
			first := matrixTailUntilMatched(t, server, tail)

			matrixDDL(t, server, "postgres", "SELECT pg_rotate_logfile()")
			time.Sleep(time.Second)

			matrixGenerateDeadlock(t, server)
			matrixLogMarker(t, server)

			var (
				rotated bool
				body    strings.Builder
			)

			for range 20 {
				matrixLogMarker(t, server)

				block := tail.sample()
				body.WriteString(block.body)

				if block.fields["rotated"] == "true" {
					rotated = true
				}

				if rotated && strings.Contains(body.String(), "deadlock detected") {
					break
				}

				time.Sleep(500 * time.Millisecond)
			}

			assert.True(t, rotated, "the tail follows the rotation through the route that resolved it")
			assert.Equal(t, 1, strings.Count(body.String(), "deadlock detected"),
				"the second event, once and only once - the old handle is drained before the "+
					"new file is opened, and nothing is read twice")
			assert.Equal(t, 1, strings.Count(first.body, "deadlock detected"))
		})
	}
}

func TestMatrixLogTailStructuredFormats(t *testing.T) {
	for _, format := range []logFormat{logFormatCSV, logFormatJSON} {
		for _, server := range matrixServers {
			requireMatrixLogDir(t, server)

			t.Run(fmt.Sprintf("%s/pg%d", format, server.major), func(t *testing.T) {
				if format == logFormatJSON && server.major < 15 {
					t.Skip(`jsonlog is PostgreSQL 15+: 14 answers "log format \"jsonlog\" is not supported"`)
				}

				matrixDeadlockTable(t, server)

				matrixDDL(t, server, "postgres",
					fmt.Sprintf("ALTER SYSTEM SET log_destination = '%s'", format),
					"SELECT pg_reload_conf()")

				t.Cleanup(func() {
					matrixDDL(t, server, "postgres",
						"ALTER SYSTEM RESET log_destination", "SELECT pg_reload_conf()")
				})

				time.Sleep(2 * time.Second)

				tail := newMatrixTail(t, matrixTarget(server, matrixSuperuser(t)), NewDeadlocks())

				block := tail.sample()
				require.Equal(t, string(format), block.fields["log_format"],
					"the destination the cluster declares, not the one it defaults to")
				require.Equal(t, matchedBySQLState, block.fields["matched_by"],
					"a five-character error code is exact, locale-independent and version-independent")

				matrixGenerateDeadlock(t, server)

				matched := matrixTailUntilMatched(t, server, tail)

				assert.Contains(t, matched.body, "deadlock detected")
				assert.Contains(t, matched.body, "40P01")

				size, err := strconv.Atoi(matched.fields["bytes"])
				require.NoError(t, err)
				assert.Equal(t, size, len(matched.body))
			})
		}
	}
}

func TestMatrixLogTailUnreadable(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			requireUnprivileged(t)

			tail := newMatrixTail(t, matrixTarget(server, matrixSuperuser(t)), NewDeadlocks())
			require.Equal(t, LogAccessDirect, tail.sample().fields["log_access"])

			matrixDDL(t, server, "postgres", "SELECT pg_rotate_logfile()")
			time.Sleep(2 * time.Second)

			current, err := matrixCurrentLogfile(t, matrixTarget(server, matrixSuperuser(t)))
			require.NoError(t, err)

			matrixMakeUnreadable(t, server, current)

			var unreadable textBlock

			for range 10 {
				block := tail.sample()
				if block.fields["reason"] == reasonUnreadable {
					unreadable = block
					break
				}

				time.Sleep(500 * time.Millisecond)
			}

			require.NotEmpty(t, unreadable.header, "the tail never reported the unreadable file")

			assert.Equal(t, current, unreadable.fields["log_path"], "and it names what it could not open")
			assert.Equal(t, "false", unreadable.fields["log_readable"])
			assert.False(t, unreadable.has("matched"),
				"the outcome of a correct-looking Mode H deployment, and it is never a zero")
		})
	}
}

func matrixMakeUnreadable(t *testing.T, server matrixServer, path string) {
	t.Helper()

	restore := func() {
		_ = os.Chmod(path, 0o644)
		matrixChmodInContainer(t, server, path, "0644", false)
	}

	_ = os.Chmod(path, 0)
	if !isReadable(path) {
		t.Cleanup(restore)
		return
	}

	matrixChmodInContainer(t, server, path, "0000", true)
	if !isReadable(path) {
		t.Cleanup(restore)
		return
	}

	restore()
	t.Skipf("neither this host nor the container can make %s unreadable to the test process; "+
		"manual-tests/9-log-tail.sh is where this case is checked against a real installation", path)
}

func matrixChmodInContainer(t *testing.T, server matrixServer, path, mode string, strict bool) {
	t.Helper()

	if _, err := exec.LookPath("docker"); err != nil {
		if strict {
			t.Skip("docker is not on PATH, and this case needs to chmod a file owned by the container")
		}

		return
	}

	compose := os.Getenv("YC_PG_COMPOSE")
	if compose == "" {
		compose = filepath.Join("..", "..", "..", "compose.pg.yaml")
	}

	cmd := exec.Command("docker", "compose", "-f", compose, "exec", "-T",
		fmt.Sprintf("pg%d", server.major), "chmod", mode, path)

	out, err := cmd.CombinedOutput()
	if strict {
		require.NoError(t, err, "docker compose exec chmod: %s", out)
	}
}

func TestMatrixLogSettingsAtThePrivilegeFloor(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			target := matrixTarget(server, matrixRestricted(t))
			values := assertArtifactComplete(t, target, collectFromMatrix(t, target))

			assert.Equal(t, "1440", values["log_rotation_age"])
			assert.Equal(t, "10240", values["log_rotation_size"])

			for _, name := range []string{
				"log_timezone",
				"log_min_messages",
				"log_error_verbosity",
				"log_min_error_statement",
				"log_file_mode",
			} {
				assert.NotEmpty(t, values[name],
					"%s decides what a matched= count or a reason=unreadable means, and it "+
						"reads at the floor", name)
			}

			assert.NotContains(t, splitSettingList(values["settings_unavailable"]), "log_rotation_age",
				"none of the seven is superuser-only")
		})
	}
}

// --- pg_explain.txt ----------------------------------------------------------
//
// Everything below is a fact about PostgreSQL a unit test cannot reach: the version split,
// the executor permission check, what the wire protocol leaves in pg_stat_activity, and
// what auto_explain writes.

const matrixExplainTable = "yc_explain"

// matrixExplainFixture builds the table the estimated modes plan against, in yc_second -
// the database with no pg_stat_statements view, which is also the fallback case.
func matrixExplainFixture(t *testing.T, server matrixServer) {
	t.Helper()

	matrixDDL(t, server, matrixSecondDB,
		"DROP TABLE IF EXISTS "+matrixExplainTable,
		"CREATE TABLE "+matrixExplainTable+" (id int primary key, sku text, note text)",
		"INSERT INTO "+matrixExplainTable+
			" SELECT g, 'SKU-' || g, repeat('x', 16) FROM generate_series(1, 400) g",
		"ANALYZE "+matrixExplainTable)

	t.Cleanup(func() {
		matrixDDL(t, server, matrixSecondDB, "DROP TABLE IF EXISTS "+matrixExplainTable)
	})
}

func matrixConn(t *testing.T, target Target) *Conn {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	conn, err := Connect(ctx, target)
	require.NoError(t, err, "connect to %s", target)

	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer closeCancel()

		_ = conn.Close(closeCtx)
	})

	return conn
}

// matrixPlan submits one EXPLAIN through the collector's own submission path, so the
// protocol under test is the one production uses.
func matrixPlan(t *testing.T, conn *Conn, statement string, generic bool) (string, error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	plan, truncated, err := submitExplain(ctx, conn, statement, generic)

	assert.False(t, truncated, "the fixture's plans are far under MaxPlanBytes")

	return string(plan), err
}

func TestMatrixExplainGenericPlanIsAVersionGate(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)
			conn := matrixConn(t, target)

			ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
			defer cancel()

			var (
				database              string
				dbid                  *string
				hasPgStatCheckpointer bool
				hasGenericPlan        bool
			)
			require.NoError(t, conn.QueryRow(ctx, currentDatabaseSQL).
				Scan(&database, &dbid, &hasPgStatCheckpointer, &hasGenericPlan))

			assert.Equal(t, server.major >= 16, hasGenericPlan,
				"EXPLAIN (GENERIC_PLAN) landed in PostgreSQL 16")

			parameterized := "SELECT * FROM " + matrixExplainTable + " WHERE id = $1"

			plan, err := matrixPlan(t, conn,
				explainStatement(explainOptions(true), parameterized),
				true)

			if hasGenericPlan {
				require.NoError(t, err)
				assert.Contains(t, plan, matrixExplainTable)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), "there is no parameter $1")
			assert.NotContains(t, err.Error(), "generic_plan")

			_, optionErr := matrixPlan(t, conn,
				explainStatement(explainOptions(true), "SELECT 1"),
				true)

			require.Error(t, optionErr)
			assert.Contains(t, optionErr.Error(), `unrecognized EXPLAIN option "generic_plan"`,
				"the option error exists on 14/15 - it is simply unreachable for the "+
					"parameterized text this mode submits")
		})
	}
}

func TestMatrixExplainPrivilege(t *testing.T) {
	const reader = "yc_explain_reader"

	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			selectPlan := explainStatement(explainOptions(false),
				"SELECT * FROM "+matrixExplainTable+" WHERE id = 1")

			for _, role := range matrixRoles {
				if role.superuser {
					continue
				}

				conn := matrixConn(t, matrixTargetDB(server, role, matrixSecondDB))

				_, err := matrixPlan(t, conn, selectPlan, false)

				require.Error(t, err, "%s must not be able to plan an application query", role.user)
				assert.Contains(t, err.Error(), "permission denied",
					"%s: pg_monitor is a statistics grant, not a data one", role.user)
			}

			matrixDDL(t, server, matrixSecondDB,
				"DROP ROLE IF EXISTS "+reader,
				"CREATE ROLE "+reader+" LOGIN PASSWORD 'yc-reader-pw'",
				"GRANT pg_read_all_data TO "+reader)

			t.Cleanup(func() {
				matrixDDL(t, server, matrixSecondDB, "DROP ROLE IF EXISTS "+reader)
			})

			target := matrixTargetDB(server, matrixRole{user: reader, password: "yc-reader-pw"},
				matrixSecondDB)
			conn := matrixConn(t, target)

			plan, err := matrixPlan(t, conn, selectPlan, false)
			require.NoError(t, err, "pg_read_all_data plans a SELECT")
			assert.Contains(t, plan, matrixExplainTable)

			_, writeErr := matrixPlan(t, conn, explainStatement(explainOptions(false),
				"UPDATE "+matrixExplainTable+" SET note = 'x' WHERE id = 1"),
				false)

			require.Error(t, writeErr,
				"EXPLAIN runs the executor's permission checks, so a read-only grant "+
					"cannot plan a write - and the answer is not to grant writes to a "+
					"monitoring role")
			assert.Contains(t, writeErr.Error(), "permission denied")
		})
	}
}

func TestMatrixExplainQueryIDIsPopulated(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			require.Nil(t, matrixQuery(t,
				matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB),
				"SELECT to_regclass('pg_stat_statements')::text")[0],
				"yc_second has no extension view, which is the fallback's own case")

			workload := matrixLogConn(t, server, matrixSecondDB)
			require.NoError(t, matrixLogExec(t, workload,
				"SELECT count(*) FROM "+matrixExplainTable))

			rows := matrixQuery(t, matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB),
				`SELECT a.state, a.query, a.query_id::text
				   FROM pg_catalog.pg_stat_activity a
				  WHERE a.datname = $1 AND a.backend_type = 'client backend'
				    AND a.query LIKE '%`+matrixExplainTable+`%'
				    AND a.pid <> pg_backend_pid()
				  ORDER BY a.state_change DESC LIMIT 3`, matrixSecondDB)

			require.NotNil(t, rows[0], "the workload session is still connected")
			assert.Equal(t, "idle", *rows[0])
			require.NotNil(t, rows[2],
				"query_id is populated under the default compute_query_id=auto, which "+
					"preloading pg_stat_statements turns on - extension view or not")
			assert.NotEqual(t, "0", *rows[2])
		})
	}
}

func TestMatrixExplainAutoExplainEntries(t *testing.T) {
	for _, server := range matrixServers {
		requireMatrixLogDir(t, server)

		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			for _, tc := range []struct {
				name    string
				verbose string
				analyze string
			}{
				{name: "verbose=off analyze=off", verbose: "off", analyze: "off"},
				{name: "verbose=on analyze=off", verbose: "on", analyze: "off"},
				{name: "verbose=on analyze=on", verbose: "on", analyze: "on"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tail := matrixExplainTailAtEnd(t, server)

					workload := matrixLogConn(t, server, matrixSecondDB)
					for _, sql := range []string{
						"LOAD 'auto_explain'",
						"SET auto_explain.log_min_duration = 0",
						"SET auto_explain.log_verbose = " + tc.verbose,
						"SET auto_explain.log_analyze = " + tc.analyze,
						"SELECT count(*) FROM " + matrixExplainTable,
					} {
						require.NoError(t, matrixLogExec(t, workload, sql),
							"a session-scoped LOAD needs no compose change and no restart")
					}

					event := matrixExplainUntilStored(t, server, tail)

					assert.Contains(t, event, "Query Text:",
						"present on every version and both verbose states")
					assert.Contains(t, event, matrixExplainTable)

					identifier := planQueryIdentifier([]byte(event))

					switch {
					case tc.verbose == "on" && server.major >= 16:
						assert.NotEmpty(t, identifier,
							"the join key the LOGGED mode attaches by")

					default:
						assert.Empty(t, identifier,
							"log_verbose=off anywhere, and PostgreSQL 14/15 even with it "+
								"on, emit no identifier - the mode degrades to unattached "+
								"plans rather than guessing by query text")
					}

					if tc.analyze == "on" {
						assert.Contains(t, event, "actual",
							"log_analyze is what buys timings; it is off by default, which "+
								"is why the mode's default value is authenticity")
					} else {
						assert.NotContains(t, event, "actual time",
							"the default is cost-only")
					}
				})
			}
		})
	}
}

// matrixExplainTailAtEnd opens a tail with the artifact's own matcher, positioned past
// everything already in the log.
func matrixExplainTailAtEnd(t *testing.T, server matrixServer) *logTail {
	t.Helper()

	conn := matrixConn(t, matrixTarget(server, matrixMonitor(t)))

	built := newLogTail("pg_explain", explainMatch)
	tail := &built

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	require.True(t, tail.openAtEnd(ctx, conn, SampleContext{
		At: time.Now(), Index: 1, Total: 2, Database: "postgres",
		redact: func(err error) string { return errorText(err, "") },
	}), "reason=%s log_access=%s", tail.source.reason, tail.source.logAccess())

	t.Cleanup(tail.closeFile)

	return tail
}

func matrixExplainUntilStored(t *testing.T, server matrixServer, tail *logTail) string {
	t.Helper()

	deadline := time.Now().Add(20 * time.Second)

	for {
		matrixLogMarker(t, server)

		events, _ := tail.readEvents(context.Background(), nil, time.Time{})
		if len(events) > 0 {
			require.Len(t, events, 1, "one statement ran, so one plan was logged")

			return string(events[0])
		}

		if time.Now().After(deadline) {
			t.Fatal("no auto_explain entry reached the log within the deadline")
		}

		time.Sleep(500 * time.Millisecond)
	}
}

func TestMatrixExplainWireProtocol(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)
			workload := matrixConn(t, target)

			ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
			defer cancel()

			const statement = "SELECT sku FROM " + matrixExplainTable + " WHERE id = $1"

			rows, err := workload.Query(ctx, statement, 42)
			require.NoError(t, err)
			rows.Close()
			require.NoError(t, rows.Err())

			activity := matrixQuery(t, target,
				`SELECT a.query, a.query_id::text
				   FROM pg_catalog.pg_stat_activity a
				  WHERE a.datname = $1 AND a.query LIKE '%`+matrixExplainTable+`%'
				    AND a.query LIKE '%$1%' AND a.pid <> pg_backend_pid()
				  ORDER BY a.state_change DESC LIMIT 2`, matrixSecondDB)

			require.NotNil(t, activity[0])
			assert.Contains(t, *activity[0], "$1",
				"the parameterized text, which plain EXPLAIN refuses - the gate that "+
					"routes this candidate to the generic mode instead")
			require.NotNil(t, activity[1])

			_, bindErr := matrixPlan(t, workload,
				explainStatement(explainOptions(true), statement), false)

			require.Error(t, bindErr, "an unbound $1 cannot reach the server over Bind")

			if server.major >= 16 {
				plan, simpleErr := matrixPlan(t, workload,
					explainStatement(explainOptions(true), statement),
					true)

				require.NoError(t, simpleErr, "the same statement, over the protocol psql uses")
				assert.Contains(t, plan, matrixExplainTable)
			}
		})
	}
}

func TestMatrixExplainActivityTextTruncation(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

			size := matrixQuery(t, target,
				"SELECT setting FROM pg_catalog.pg_settings WHERE name = 'track_activity_query_size'")
			require.NotNil(t, size[0])

			limit, err := strconv.Atoi(*size[0])
			require.NoError(t, err)

			workload := matrixLogConn(t, server, matrixSecondDB)
			require.NoError(t, matrixLogExec(t, workload,
				"SELECT count(*) FROM "+matrixExplainTable+
					" /* "+strings.Repeat("x", limit+500)+" */"))

			read := matrixQuery(t, target,
				`SELECT octet_length(a.query)::text, a.query
				   FROM pg_catalog.pg_stat_activity a
				  WHERE a.datname = $1 AND a.query LIKE '%`+matrixExplainTable+`%'
				    AND a.pid <> pg_backend_pid()
				  ORDER BY octet_length(a.query) DESC LIMIT 1`, matrixSecondDB)

			require.NotNil(t, read[0])
			assert.Equal(t, strconv.Itoa(limit-1), *read[0],
				"cut at the cap less one, and nothing in the text says so")

			require.NotNil(t, read[1])
			assert.NotContains(t, *read[1], "...", "unmarked, which is the whole problem")

			assert.True(t,
				truncatedActivityText(
					activityRow{queryBytes: ptr(int64(limit - 1))},
					activityFacts{activityQuerySize: int64(limit)}),
				"and the gate the artifact applies agrees with the server about it")
		})
	}
}

func TestMatrixExplainSurvivesAStatementTimeout(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			conn := matrixConn(t, matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB))

			ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
			defer cancel()

			runUtilityStatement(ctx, conn, "SET statement_timeout TO '150ms'")

			rows, err := conn.Query(ctx, "SELECT pg_sleep(2)")
			if err == nil {
				for rows.Next() { //nolint:revive // draining is the point
				}

				rows.Close()
				err = rows.Err()
			}

			require.Error(t, err, "the statement was expected to be cancelled")

			assert.Contains(t, err.Error(), "57014",
				"query_canceled, raised by the server rather than by a client deadline - "+
					"a client-context expiry would have closed this connection instead")

			runUtilityStatement(ctx, conn, resetExplainTimeoutSQL)

			plan, planErr := matrixPlan(t, conn, explainStatement(explainOptions(false),
				"SELECT * FROM "+matrixExplainTable+" WHERE id = 1"), false)

			require.NoError(t, planErr, "the next candidate runs on the same connection")
			assert.Contains(t, plan, matrixExplainTable)
		})
	}
}

func TestMatrixExplainActivityTruncationIsMultibyteAware(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

			size := matrixQuery(t, target,
				"SELECT setting FROM pg_catalog.pg_settings WHERE name = 'track_activity_query_size'")
			require.NotNil(t, size[0])

			limit, err := strconv.Atoi(*size[0])
			require.NoError(t, err)

			maxChar := matrixQuery(t, target,
				`SELECT pg_catalog.pg_encoding_max_length(
				            pg_catalog.pg_char_to_encoding(current_setting('server_encoding')))::text`)
			require.NotNil(t, maxChar[0])

			width, err := strconv.Atoi(*maxChar[0])
			require.NoError(t, err)
			require.Equal(t, 4, width, "the matrix databases are UTF-8")

			for offset := range 4 {
				t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
					marker := "yc_mb" + strconv.Itoa(offset)

					workload := matrixLogConn(t, server, matrixSecondDB)
					require.NoError(t, matrixLogExec(t, workload,
						"SELECT count(*) FROM "+matrixExplainTable+
							" /* "+marker+strings.Repeat("x", offset)+
							strings.Repeat("\U0001F600", limit)+" */"))

					read := matrixQuery(t, target,
						`SELECT octet_length(a.query)::text
						   FROM pg_catalog.pg_stat_activity a
						  WHERE a.datname = $1 AND a.query LIKE '%' || $2 || '%'
						    AND a.pid <> pg_backend_pid() LIMIT 1`, matrixSecondDB, marker)

					require.NotNil(t, read[0])

					octets, err := strconv.Atoi(*read[0])
					require.NoError(t, err)

					require.Less(t, octets, limit,
						"the statement was far longer than the cap, so this is a cut prefix")

					assert.True(t,
						truncatedActivityText(
							activityRow{queryBytes: ptr(int64(octets))},
							activityFacts{
								activityQuerySize: int64(limit),
								maxCharBytes:      int64(width),
							}),
						"a cut prefix at %d octets under a %d-byte cap must not read as "+
							"complete text - it would be submitted mid-statement", octets, limit)
				})
			}
		})
	}
}

func TestMatrixExplainReadsItsOwnOIDForAQuotedRoleName(t *testing.T) {
	const quotedRole = "yc_explain@review.test"

	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixDDL(t, server, matrixSecondDB,
				fmt.Sprintf("DROP ROLE IF EXISTS %q", quotedRole),
				fmt.Sprintf("CREATE ROLE %q LOGIN PASSWORD 'yc-review-pw'", quotedRole),
				fmt.Sprintf("GRANT pg_monitor TO %q", quotedRole))

			t.Cleanup(func() {
				matrixDDL(t, server, matrixSecondDB,
					fmt.Sprintf("DROP ROLE IF EXISTS %q", quotedRole))
			})

			target := matrixTargetDB(server,
				matrixRole{user: quotedRole, password: "yc-review-pw"}, matrixSecondDB)

			conn := matrixConn(t, target)

			ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
			defer cancel()

			_, facts, err := readActivity(ctx, conn)

			require.NoError(t, err,
				"the whole read, facts included, fails on a role name that needs quoting")
			assert.True(t, facts.read)
			assert.NotEmpty(t, facts.selfOID, "self-exclusion needs the capture role's OID")
			assert.Positive(t, facts.activityQuerySize)
			assert.Positive(t, facts.maxCharBytes)

			rows, err := conn.Query(ctx, "SELECT (current_user::text)::regrole::oid::text")
			if err == nil {
				rows.Close()
				err = rows.Err()
			}

			require.Error(t, err,
				"current_user::text::regrole is what this role name breaks; if it stopped "+
					"breaking, PostgreSQL changed and activitySQL can be simplified")
		})
	}
}

func TestMatrixExplainRefusesAMultiStatementBatch(t *testing.T) {
	for _, server := range matrixServers {
		t.Run(fmt.Sprintf("pg%d", server.major), func(t *testing.T) {
			matrixExplainFixture(t, server)

			target := matrixTargetDB(server, matrixSuperuser(t), matrixSecondDB)

			workload := matrixLogConn(t, server, matrixSecondDB)
			require.NoError(t, matrixLogExec(t, workload,
				"SET application_name = 'yc_batch'; SELECT count(*) FROM "+matrixExplainTable))

			batch := matrixQuery(t, target,
				`SELECT a.query FROM pg_catalog.pg_stat_activity a
				  WHERE a.datname = $1 AND a.application_name = 'yc_batch'
				    AND a.pid <> pg_backend_pid() LIMIT 1`, matrixSecondDB)

			require.NotNil(t, batch[0])
			assert.Contains(t, *batch[0], ";",
				"the whole batch reads back as one activity text")
			assert.Contains(t, *batch[0], "SET application_name")

			conn := matrixConn(t, target)

			_, err := matrixPlan(t, conn,
				explainStatement(explainOptions(false), *batch[0]), false)

			require.Error(t, err,
				"cleanly refused: over the simple protocol this would have run the "+
					"customer's statements, which is the one thing this artifact must never do")
		})
	}
}
