//go:build pgintegration

package postgres

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The version-and-privilege matrix: compose.pg.yaml's five servers, each read
// by three roles. Opt-in via the pgintegration build tag.
//
// Everything asserted here is a claim the rest of the package makes and cannot
// prove about itself: a fake Querier agrees with whatever the test author
// believed about a running server. Two such rules were wrong when first written
// and both were found here - pg_current_logfile()'s EXECUTE grant to pg_monitor
// landed in PostgreSQL 17, and data_directory used to ride in that same
// statement and went down with it.

// One of compose.pg.yaml's containers.
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

// One of the roles pg_matrix_init.sql creates, plus the image's own superuser.
type matrixRole struct {
	user     string
	password string

	// monitor is a member of pg_monitor; superuser is the built-in role. A role
	// that is neither holds LOGIN and nothing else.
	superuser bool
	monitor   bool
}

var matrixRoles = []matrixRole{
	{user: "postgres", password: "yc-superuser-pw", superuser: true},
	{user: "yc_monitor", password: "yc-monitor-pw", monitor: true},
	{user: "yc_restricted", password: "yc-restricted-pw"},
}

// Can the role see the superuser-only settings? pg_monitor includes
// pg_read_all_settings, and a superuser sees everything.
func (r matrixRole) privileged() bool { return r.superuser || r.monitor }

// The four entries in capturedSettings that guc_tables.c marks
// GUC_SUPERUSER_ONLY.
var superuserOnlySettings = []string{
	"data_directory",
	"log_directory",
	"log_filename",
	"shared_preload_libraries",
}

// Overridable for a runner that reaches the servers by service name rather than
// on loopback. Deliberately not spelled PGHOST: Connect clears the libpq
// environment before parsing, so that name could not reach the connection.
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

		// The containers serve no TLS. Every other consumer of this package
		// defaults to require; the matrix is testing catalog behaviour, not
		// transport.
		SSLMode: "disable",
	}
}

// Runs a real capture against one server as one role.
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

// The every-key rule stated where it can actually fail: the keys a reader gets
// must not depend on the server version or on the role's privileges.
func assertArtifactComplete(t *testing.T, target Target, m Metadata) map[string]string {
	t.Helper()

	// Stamped the way the capture adapter stamps it, and with a fixed value so
	// a release bump cannot change what this test compares.
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

// The claim that costs the most if it is wrong: serverFactsSQL cannot fail for
// want of a grant. A non-empty query_error for the restricted role sinks the
// whole privilege-resilience design, and every later artifact inherits it.
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

	// inet_server_addr() is NULL over a Unix socket and set over TCP, which is
	// how the matrix connects.
	assert.NotEmpty(t, values["inet_server_addr"])

	// The server's own port, not the client's view of it: a consumer reading
	// this as "where the agent connected to" gets a wrong answer through any
	// port mapping, containers included.
	assert.Equal(t, "5432", values["inet_server_port"])
	assert.NotEqual(t, strconv.Itoa(server.port), values["inet_server_port"])

	assert.Equal(t, strconv.FormatBool(role.superuser || role.monitor), values["has_pg_monitor_role"],
		"pg_has_role(..., 'member') is true for a superuser as well as for a member")
}

// The version half of the matrix: the facts that differ across the supported
// range, checked against the range rather than against what the agent believes.
func assertCapabilities(t *testing.T, server matrixServer, values map[string]string) {
	t.Helper()

	// pg_stat_bgwriter's checkpointer counters moved into pg_stat_checkpointer
	// in PostgreSQL 17. This is the probe that stops the next artifact reading
	// a view that does not exist.
	assert.Equal(t, strconv.FormatBool(server.major >= 17), values["has_pg_stat_checkpointer"],
		"pg_stat_checkpointer landed in PostgreSQL 17")

	// sessions_fatal landed in PostgreSQL 14, so no server here exercises the
	// probe's false branch.
	assert.Equal(t, "true", values["has_session_fatal_stats"])

	// The extension's schema version moves independently of the server, which is
	// why extversion is read rather than inferred from server_version_num. These
	// are fresh containers, so it is whatever each image ships.
	assert.Equal(t, "true", values["has_pg_stat_statements"])
	assert.NotEmpty(t, values["pg_stat_statements_version"])
	t.Logf("pg%d: pg_stat_statements %s, server_version_num %s",
		server.major, values["pg_stat_statements_version"], values["server_version_num"])
}

// The privilege half: a role that may not read the superuser-only settings must
// still get a complete artifact, with those four values empty and named in
// settings_unavailable, rather than an error costing the whole statement.
func assertSettingsVisibility(t *testing.T, role matrixRole, values map[string]string) {
	t.Helper()

	// Readable by every role, so they anchor the assertions below: were these
	// empty too, the role would be failing to read pg_settings at all rather
	// than being filtered by it.
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

	// The least-privilege floor. settings_unavailable is what distinguishes
	// "shared_preload_libraries is empty because nothing is configured" from
	// "empty because this role may not see it".
	unavailable := splitSettingList(values["settings_unavailable"])
	assert.ElementsMatch(t, superuserOnlySettings, unavailable,
		"exactly the superuser-only settings are omitted, and they are named")

	for _, name := range superuserOnlySettings {
		assert.Empty(t, values[name], "%s must be written empty rather than erroring the statement", name)
	}
}

// pg_current_logfile() has EXECUTE revoked from PUBLIC, and the grant to
// pg_monitor only landed in PostgreSQL 17, so on 14-16 the recommended role is
// denied - the normal outcome on three of the five supported majors.
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

		// The whole point of moving data_directory out of this statement: its
		// denial must cost the capture mode and nothing else.
		if role.privileged() {
			assert.NotEmpty(t, values["data_directory"],
				"data_directory rides in the settings catalogue and survives this denial")
		}

		return
	}

	assert.Empty(t, values["current_logfile_error"])
	assert.NotEmpty(t, values["current_logfile"], "logging_collector is on, so there is a current log file")

	// Resolved against data_directory, because the collector's default
	// log_directory is relative.
	assert.NotEmpty(t, values["current_logfile_resolved"])

	// Deliberately not asserted as pg-dbhost: the resolved path is inside the
	// container, so readability depends on where the test runs. What matters is
	// that detection reached a conclusion at all.
	assert.NotEqual(t, ModeUnknown, values["capture_mode"])
	assert.NotEmpty(t, values["current_logfile_readable"])
}

// Measures the premise behind replicationSQL's isolation, which was reasoned
// rather than measured when the split was designed.
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
