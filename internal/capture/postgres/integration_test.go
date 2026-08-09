//go:build pgintegration

package postgres

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
				assertReplicationProbe(t, values)
			})
		}
	}
}

// matrixCaptureSessionsSQL counts this capture's backends, excluding the
// connection doing the counting - which carries the same application_name,
// because every connection this package opens does.
const matrixCaptureSessionsSQL = `SELECT count(*)
FROM pg_catalog.pg_stat_activity
WHERE application_name = $1 AND pid <> pg_backend_pid()`

// matrixMetadataWindow is long enough for the count below to land inside it.
const matrixMetadataWindow = 3 * time.Second

// runMatrixMetadataWindow runs the collector through a real window and, from a
// second connection while that window is open, counts how many sessions the
// capture is holding.
func runMatrixMetadataWindow(t *testing.T, target Target) ([]ArtifactResult, int64) {
	t.Helper()
	t.Chdir(t.TempDir())

	window := &Window{
		Duration: matrixMetadataWindow,
		Target:   target,
		Collectors: []Collector{
			NewMetadata(target, "matrix", time.Now()),
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

// TestMatrixMetadataWindow is the empirical form of the claim this slice makes:
// routing pg_metadata.txt through the window changed what writes it and nothing
// about what it says. TestMatrix above still pins Collect itself, on the same
// sweep, by calling it directly.
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

				// The frame moved and the content did not - asserted against a
				// real server rather than against the agent's own fixtures.
				_, directValues, directKeys := parseArtifact(t, writeArtifact(t, direct))
				assert.Equal(t, directKeys, keys,
					"the window writes exactly the keys the direct call does")

				assert.Equal(t, directValues["capture_mode"], values["capture_mode"],
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

	if role.privileged() {
		assert.Empty(t, values["settings_unavailable"],
			"a role with pg_read_all_settings sees every setting in the catalogue")

		for _, name := range superuserOnlySettings {
			assert.NotEmpty(t, values[name], "%s is visible to this role", name)
		}

		assert.Contains(t, values["shared_preload_libraries"], "pg_stat_statements")

		return
	}

	unavailable := splitSettingList(values["settings_unavailable"])
	assert.ElementsMatch(t, superuserOnlySettings, unavailable,
		"exactly the superuser-only settings are omitted, and they are named")

	for _, name := range superuserOnlySettings {
		assert.Empty(t, values[name], "%s must be written empty rather than erroring the statement", name)
	}
}

func assertLogLocation(t *testing.T, server matrixServer, role matrixRole, values map[string]string) {
	t.Helper()

	allowed := role.superuser || (role.monitor && server.major >= 17)

	t.Logf("pg%d/%s: capture_mode=%s current_logfile=%q error=%q",
		server.major, role.user, values["capture_mode"],
		values["current_logfile"], values["current_logfile_error"])

	if !allowed {
		assert.NotEmpty(t, values["current_logfile_error"],
			"pg_current_logfile() is denied to this role on PostgreSQL %d", server.major)
		assert.Equal(t, ModeUnknown, values["capture_mode"],
			"a denied probe leaves the mode unknown rather than guessing at it")
		assert.Empty(t, values["current_logfile"])

		if role.privileged() {
			assert.NotEmpty(t, values["data_directory"],
				"data_directory rides in the settings catalogue and survives this denial")
		}

		return
	}

	assert.Empty(t, values["current_logfile_error"])
	assert.NotEmpty(t, values["current_logfile"], "logging_collector is on, so there is a current log file")

	assert.NotEmpty(t, values["current_logfile_resolved"])

	assert.NotEqual(t, ModeUnknown, values["capture_mode"])
	assert.NotEmpty(t, values["current_logfile_readable"])
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

// sampleBlock is one collector's block, keyed by its first column - relid for
// pg_bloat.txt, datid for pg_health.txt, which is the join key either way.
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

// parseSampleBlocks reads the blocks one view produced, skipping the window's
// own preamble and closing block.
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

// assertMatrixDeadTuples checks the fixture's dead tuples against the block's
// own n_tup_upd rather than against the bare constant, so the two captured
// columns cross-check each other.
//
// The constant alone is wrong in both directions once a container has been up
// for a while. A stray UPDATE - a hand-run query between test runs - leaves its
// old row version dead too, so the count climbs above matrixOrdersDead; and
// PostgreSQL's opportunistic page pruning can reclaim those update-made dead
// tuples again on ordinary access, so it can fall back. Neither says anything
// about the reading, and both broke this assertion on the pg16 container.
//
// What must not happen still fails here: a vacuum would take the DELETE's dead
// tuples with it, dropping the count below the lower bound. On a container
// nothing has written to, updated is 0 and the two bounds collapse to the exact
// count the fixture makes.
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

// matrixHealthSamples is three blocks at a 1s cadence over a 3s window, so five
// servers times three roles stays under a minute.
const matrixHealthSamples = 3

// matrixDatabases is what pg_stat_database returns on a matrix container: the
// shared-objects row, both templates, postgres, and yc_second.
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

// matrixKeepCommitting holds a second connection open and commits on it until
// the returned function is called, so xact_commit moves while the window is
// sampling. Without it a backend flushes its statistics at most once per second
// (PGSTAT_MIN_INTERVAL on 15 and later), so three identical reads in an idle
// container would be a legitimate outcome.
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

// assertMatrixHealthCapKeepsTheProtectedRows lowers the cap while connected to
// the database with the highest OID - the case a plain ORDER BY datid gets
// wrong.
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
		database string
		dbid     *string
	)
	require.NoError(t, conn.QueryRow(ctx, currentDatabaseSQL).Scan(&database, &dbid))
	require.Equal(t, "yc_second", database)
	require.NotNil(t, dbid)

	// Exactly the two rows the inner ordering protects; anything the cap kept
	// beyond them would weaken the assertion below.
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

// matrixDatidOf is the datid of the row for datname, which must be present.
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
