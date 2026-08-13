//go:build pgintegration

package postgres

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
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
	)
	require.NoError(t, conn.QueryRow(ctx, currentDatabaseSQL).Scan(&database, &dbid, &hasPgStatCheckpointer))
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

// The replication matrix creates and drops its own slots and WAL sender, so
// pg_matrix_init.sql stays untouched and no other matrix test has to
// re-baseline.

const matrixReplicationWindow = 2 * time.Second

const matrixWALSenderName = "yc-360-matrix-walsender"

const (
	matrixPlainSlot    = "yc_matrix_plain"
	matrixReservedSlot = "yc_matrix_reserved"
)

// matrixOptionalColumns is which of the six optional pg_replication_slots
// columns each version has. Empty means the header key is absent rather than
// empty: string_agg over no matching rows is NULL, and a NULL is never written.
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

// matrixStartWALSender gives pg_stat_replication a row, with no standby and no
// second container.
//
// replication=database rather than replication=true: a logical replication
// connection names a database, so pg_hba's `host all all all` matches it, where
// a physical one needs a `host replication` line the postgres image does not add
// for connections from outside the container. It still streams physically,
// which is what puts state=streaming and a sent_lsn on the row.
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

	// IDENTIFY_SYSTEM's third column is the current WAL position: start from
	// there rather than from 0/0, which would ask the server for WAL it has
	// long recycled.
	lsn := string(identified[0].Rows[0][2])

	streaming := make(chan struct{})

	go func() {
		defer close(streaming)

		// Runs until the context is cancelled. The error on the way out is the
		// teardown rather than a failure, so it is deliberately not asserted.
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

// matrixCreateSlot creates one physical slot and returns its drop. Without
// immediately_reserve the slot has NULL restart_lsn and NULL wal_status
// together, which is why the round-trip needs two slots rather than one.
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

// matrixSenderViewColumns is pg_stat_replication's own column set, which the
// artifact renames three of. Recorded here rather than derived from
// replicationColumns, so that comparing the two is a measurement.
var matrixSenderViewColumns = []string{
	"pid", "usesysid", "usename", "application_name", "client_addr",
	"client_hostname", "client_port", "backend_start", "backend_xmin", "state",
	"sent_lsn", "write_lsn", "flush_lsn", "replay_lsn",
	"write_lag", "flush_lag", "replay_lag",
	"sync_priority", "sync_state", "reply_time",
}

// matrixSlotViewColumns is pg_replication_slots' column set on one version: the
// stable fifteen plus whichever optional ones this version has.
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

// assertMatrixViewColumns asks the server what each view holds, which is the
// only form of this check that catches a column being added. The artifact reads
// a fixed list, so its own header is identical across versions by construction;
// a missing column surfaces as error= on the block, but a new one is silent.
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

// assertMatrixMasking covers the one privilege finding here that is silent:
// every other costs a statement and says so in an error= header, where this one
// returns the row and empties the columns carrying the answer.
func assertMatrixMasking(t *testing.T, block capacityMatrixBlock, role matrixRole) {
	t.Helper()

	row := block.rowWhere(t, "application_name", matrixWALSenderName)

	// The columns that survive masking. pid being one is what makes a
	// non-pointer scan destination safe.
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
