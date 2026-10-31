//go:build mysqlintegration

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The matrix of compose.mysql.yaml. Opt-in, as the postgres matrix is:
//
//	scripts/mysql-matrix-setup.sh
//	docker compose -f compose.mysql.yaml up -d --wait
//	go test -tags mysqlintegration ./internal/capture/mysql/...
//
// YC_MY_MATRIX_SERVERS limits the run to the named services, for a machine that
// cannot hold all five.

type matrixServer struct {
	service string
	port    int

	// What the server's version and command line make each flag.
	cpuTime          bool
	cpuReason        string
	memoryTracking   bool
	memoryInstrument bool
	printAll         bool
	variant          string
}

var matrixServers = []matrixServer{
	{service: "my80", port: 13380, cpuReason: cpuReasonConsumers, memoryTracking: true, printAll: true, variant: VariantMySQL},
	{service: "my80old", port: 13370, cpuReason: cpuReasonVersion, memoryInstrument: true, variant: VariantMySQL},
	{service: "my84", port: 13384, cpuTime: true, memoryTracking: true, memoryInstrument: true, printAll: true, variant: VariantMySQL},
	{service: "my9", port: 13390, cpuTime: true, memoryTracking: true, memoryInstrument: true, printAll: true, variant: VariantMySQL},
	{service: "percona84", port: 13394, cpuReason: cpuReasonConsumers, memoryTracking: true, memoryInstrument: true, printAll: true, variant: VariantPercona},
}

type matrixRole struct {
	user, password string
	privileged     bool
}

var matrixRoles = []matrixRole{
	{user: "root", password: "yc-root-pw", privileged: true},
	{user: "yc_monitor", password: "yc-monitor-pw", privileged: true},
	{user: "yc_restricted", password: "yc-restricted-pw"},
}

func selectedServers(t *testing.T) []matrixServer {
	t.Helper()

	only := os.Getenv("YC_MY_MATRIX_SERVERS")
	if only == "" {
		return matrixServers
	}

	var selected []matrixServer

	for _, server := range matrixServers {
		if slices.Contains(strings.Split(only, ","), server.service) {
			selected = append(selected, server)
		}
	}

	return selected
}

func matrixTarget(server matrixServer, role matrixRole) Target {
	return Target{ID: server.service, Host: "127.0.0.1", Port: server.port, Username: role.user, Password: role.password}
}

// runMatrixWindow runs every collector over a short window and returns the files'
// contents by name.
func runMatrixWindow(t *testing.T, target Target, during func()) (map[string]string, []ArtifactResult) {
	t.Helper()
	t.Chdir(t.TempDir())

	metadata := NewMetadata(target, "matrix", time.Now(), "all")
	window := NewWindow(target, 12*time.Second, Collectors(metadata, "all", 4*time.Second, 6*time.Second, 6*time.Second), "")

	var wg sync.WaitGroup

	if during != nil {
		wg.Go(during)
	}

	results := window.Run(context.Background())
	wg.Wait()

	files := map[string]string{}

	for _, result := range results {
		if result.File != nil {
			result.File.Close()
		}

		content, err := os.ReadFile(result.Artifact.FileName)
		require.NoError(t, err)

		files[result.Artifact.FileName] = string(content)
	}

	return files, results
}

// rootDB is a plain connection for driving the server during a window.
func rootDB(t *testing.T, server matrixServer) *sql.DB {
	t.Helper()

	return rootDBOn(t, server, "")
}

// rootDBOn is rootDB with a default schema, which the statements it runs are
// summarised under.
func rootDBOn(t *testing.T, server matrixServer, database string) *sql.DB {
	t.Helper()

	cfg := gomysql.NewConfig()
	cfg.User, cfg.Passwd = "root", "yc-root-pw"
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("127.0.0.1:%d", server.port)
	cfg.DBName = database
	cfg.TLSConfig = "skip-verify"

	connector, err := gomysql.NewConnector(cfg)
	require.NoError(t, err)

	db := sql.OpenDB(connector)
	t.Cleanup(func() { db.Close() })

	return db
}

// forceDeadlock runs two transactions that take two rows in opposite orders.
func forceDeadlock(t *testing.T, db *sql.DB) {
	ctx := context.Background()

	run := func(first, second int, wait time.Duration) {
		conn, err := db.Conn(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = conn.ExecContext(ctx, "BEGIN")
		_, _ = conn.ExecContext(ctx, "UPDATE yc_app.orders SET status = 'dl' WHERE id = ?", first)
		time.Sleep(wait)
		_, _ = conn.ExecContext(ctx, "UPDATE yc_app.orders SET status = 'dl' WHERE id = ?", second)
		_, _ = conn.ExecContext(ctx, "COMMIT")
	}

	time.Sleep(2 * time.Second)

	var wg sync.WaitGroup

	wg.Go(func() { run(11, 12, time.Second) })
	time.Sleep(300 * time.Millisecond)
	wg.Go(func() { run(12, 11, time.Second) })
	wg.Wait()
}

func TestMatrixWindow(t *testing.T) {
	for _, server := range selectedServers(t) {
		for _, role := range matrixRoles {
			t.Run(server.service+"/"+role.user, func(t *testing.T) {
				var during func()
				if role.user == "root" {
					db := rootDB(t, server)
					during = func() { forceDeadlock(t, db) }
				}

				files, results := runMatrixWindow(t, matrixTarget(server, role), during)

				for _, result := range results {
					assert.Equal(t, StatusComplete, result.Status, "%s: %s", result.Artifact.FileName, result.Err)
				}

				metadata := files["my_metadata.txt"]
				assert.Contains(t, metadata, "\nmysql_variant,"+server.variant+"\n")
				assert.Contains(t, metadata, fmt.Sprintf("\nhas_statement_memory_tracking,%t\n", server.memoryTracking))
				assert.Contains(t, metadata, fmt.Sprintf("\nhas_process,%t\n", role.privileged))

				if role.privileged {
					assert.Contains(t, metadata, fmt.Sprintf("\nhas_cpu_time_instrument_enabled,%t\n", server.cpuTime))
					assert.Contains(t, metadata, "\ncpu_time_reason,"+server.cpuReason+"\n")
					assert.Contains(t, metadata, fmt.Sprintf("\nhas_memory_instruments_enabled,%t\n", server.memoryInstrument))
					assert.Equal(t, !server.memoryTracking || !server.memoryInstrument,
						unavailableMemory.MatchString(files["my_slow_queries.txt"]),
						"the memory columns are unavailable before 8.0.31 or with the instruments off, never zero")
					assert.Contains(t, files["my_sessions.txt"], ","+ProgramName+"\n",
						"the agent's own sessions carry their program_name")
				} else {
					assert.Contains(t, files["my_sessions.txt"], "source=innodb_trx", "the refusal is a block")
					assert.Contains(t, files["my_sessions.txt"], "reason=permission_denied")
				}

				if role.user == "root" && server.printAll {
					assert.Regexp(t, `matched=[1-9]`, files["my_deadlocks.txt"], "the forced deadlock is read from the log")
					assert.NotContains(t, files["my_deadlocks.txt"], "'dl'", "its values are replaced")
				}
			})
		}
	}
}

// unavailableMemory is a slow-queries block naming the memory columns unavailable.
var unavailableMemory = regexp.MustCompile(`unavailable=\S*max_total_memory`)

// TestMatrixMemoryInstrumentsCountTheSameInEitherQuotingMode: the count behind
// has_memory_instruments_enabled leaves out the always-on
// memory/performance_schema/% instruments whatever the server's sql_mode, which
// decides what a backslash in a string means.
func TestMatrixMemoryInstrumentsCountTheSameInEitherQuotingMode(t *testing.T) {
	for _, server := range selectedServers(t) {
		t.Run(server.service, func(t *testing.T) {
			ctx := context.Background()

			conn, err := rootDB(t, server).Conn(ctx)
			require.NoError(t, err)

			defer conn.Close()

			count := func() int {
				var n int
				require.NoError(t, conn.QueryRowContext(ctx, memoryInstrumentsSQL+" AND ENABLED = 'YES'").Scan(&n))

				return n
			}

			usual := count()

			_, err = conn.ExecContext(ctx, "SET SESSION sql_mode = 'NO_BACKSLASH_ESCAPES'")
			require.NoError(t, err)

			assert.Equal(t, usual, count())
			assert.Equal(t, server.memoryInstrument, usual > 0)
		})
	}
}

func TestMatrixPlaintextLoginNeverRequestsTheServersKey(t *testing.T) {
	for _, server := range selectedServers(t) {
		t.Run(server.service, func(t *testing.T) {
			db := rootDB(t, server)

			// The server caches a password after a login that proved it; flushing
			// is what makes the next plaintext login need the key.
			_, err := db.Exec("FLUSH PRIVILEGES")
			require.NoError(t, err)

			target := matrixTarget(server, matrixRoles[1])
			target.TLS = TLS{Disabled: true}

			ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
			defer cancel()

			_, err = Connect(ctx, target)
			assert.ErrorIs(t, err, ErrPublicKeyRetrieval)
		})
	}
}

func TestMatrixVerifiedTLS(t *testing.T) {
	server := matrixServers[2]
	if !slices.Contains(selectedServers(t), server) {
		t.Skip("my84 is the server with the test CA's certificate")
	}

	ca := filepath.Join("/tmp/yc-mytls", "ca.pem")

	ctx, cancel := context.WithTimeout(context.Background(), ModuleDeadline)
	defer cancel()

	target := matrixTarget(server, matrixRoles[1])
	target.TLS = TLS{Verify: true, CAFile: ca}

	conn, err := Connect(ctx, target)
	require.NoError(t, err, "the certificate names 127.0.0.1")
	require.NoError(t, conn.Close(ctx))

	target.TLS.ServerName = "db.example.invalid"

	_, err = Connect(ctx, target)
	assert.Error(t, err, "a certificate that does not name the server is refused")
	assert.False(t, errors.Is(err, ErrPublicKeyRetrieval))
}

// appQuery is the application statement the scenario makes the slowest, so it is
// the first digest my_explain.txt plans. SLEEP inflates its time; EXPLAIN does not
// run it.
const appQuery = "SELECT id, status, SLEEP(0.3) FROM yc_app.orders WHERE status = 'pending' AND customer_id = 7"

// preparedLiteral is held open as a server-side prepared statement for the window.
const preparedLiteral = "secret-status"

// stageBeforeWindow resets the two summaries the scenario reads - test setup the
// agent itself never does - and runs what the first sample should find.
func stageBeforeWindow(t *testing.T, root, app *sql.DB) {
	t.Helper()

	for _, statement := range []string{
		"TRUNCATE TABLE performance_schema.events_statements_summary_by_digest",
		"TRUNCATE TABLE performance_schema.table_io_waits_summary_by_index_usage",
	} {
		_, err := root.Exec(statement)
		require.NoError(t, err)
	}

	rows, err := app.Query(appQuery)
	require.NoError(t, err)
	require.NoError(t, rows.Close())

	// Full scans of the table with no index: its INDEX_NAME is NULL.
	for range 20 {
		var n int
		require.NoError(t, app.QueryRow("SELECT COUNT(*) FROM yc_app.audit_log WHERE note LIKE '%event%'").Scan(&n))
	}
}

// stageDuringWindow holds what the fast and normal samples should see: a row lock
// with a waiter, an ALTER TABLE queued for the metadata lock behind it, a prepared
// statement held open, and a statement past max_execution_time. Both waits end in
// error 1205, counted with the 3024 in my_timeouts.txt.
func stageDuringWindow(t *testing.T, root *sql.DB) {
	ctx := context.Background()

	var wg sync.WaitGroup

	time.Sleep(time.Second)

	holder, err := root.Conn(ctx)
	if !assert.NoError(t, err) {
		return
	}
	defer holder.Close()

	_, _ = holder.ExecContext(ctx, "BEGIN")
	_, _ = holder.ExecContext(ctx, "SELECT id FROM yc_app.orders WHERE id = 21 FOR UPDATE")

	wg.Go(func() {
		conn, err := root.Conn(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = conn.ExecContext(ctx, "SET SESSION innodb_lock_wait_timeout = 6")
		_, err = conn.ExecContext(ctx, "UPDATE yc_app.orders SET status = 'waited' WHERE id = 21")
		assert.ErrorContains(t, err, "1205", "the row lock waiter times out")
	})

	time.Sleep(500 * time.Millisecond)

	wg.Go(func() {
		conn, err := root.Conn(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		// A metadata-only change, which queues for the exclusive metadata lock
		// behind the open transaction and times out before it could apply.
		_, _ = conn.ExecContext(ctx, "SET SESSION lock_wait_timeout = 5")
		_, err = conn.ExecContext(ctx, "ALTER TABLE yc_app.orders COMMENT = 'yc matrix'")
		assert.ErrorContains(t, err, "1205", "the queued ALTER TABLE times out")
	})

	wg.Go(func() {
		conn, err := root.Conn(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		// The default driver configuration prepares server-side.
		stmt, err := conn.PrepareContext(ctx,
			"SELECT id FROM yc_app.orders WHERE status = '"+preparedLiteral+"' AND customer_id = ?")
		if !assert.NoError(t, err) {
			return
		}
		defer stmt.Close()

		rows, err := stmt.QueryContext(ctx, 7)
		if assert.NoError(t, err) {
			rows.Close()
		}

		time.Sleep(8 * time.Second)
	})

	wg.Go(func() {
		conn, err := root.Conn(ctx)
		if err != nil {
			return
		}
		defer conn.Close()

		_, _ = conn.ExecContext(ctx, "SET SESSION max_execution_time = 100")

		rows, err := conn.QueryContext(ctx, "SELECT SLEEP(1) FROM yc_app.orders LIMIT 2")
		if err == nil {
			for rows.Next() {
			}

			err = rows.Err()
			rows.Close()
		}

		assert.ErrorContains(t, err, "3024")
	})

	time.Sleep(6500 * time.Millisecond)

	_, _ = holder.ExecContext(ctx, "ROLLBACK")

	wg.Wait()
}

// timeoutCount is error's sum_error_raised in each sample of my_timeouts.txt.
func timeoutCount(t *testing.T, file, number string) []int {
	t.Helper()

	var counts []int

	for _, line := range strings.Split(file, "\n") {
		fields := strings.Split(line, ",")
		if len(fields) >= 3 && fields[0] == number {
			n, err := strconv.Atoi(fields[2])
			require.NoError(t, err, line)

			counts = append(counts, n)
		}
	}

	return counts
}

// blocksWithRows reports whether some sample block of source wrote rows.
func blocksWithRows(file, source string) bool {
	for _, line := range strings.Split(file, "\n") {
		if strings.Contains(line, "source="+source+" ") && strings.Contains(line, "status=OK") &&
			!strings.Contains(line, " rows=0 ") && strings.HasPrefix(line, "# sample_id=") {
			return true
		}
	}

	return false
}

func TestMatrixScenarios(t *testing.T) {
	for _, server := range selectedServers(t) {
		t.Run(server.service, func(t *testing.T) {
			root := rootDB(t, server)
			app := rootDBOn(t, server, "yc_app")

			stageBeforeWindow(t, root, app)

			files, results := runMatrixWindow(t, matrixTarget(server, matrixRoles[0]), func() {
				stageDuringWindow(t, root)
			})

			for _, result := range results {
				assert.Equal(t, StatusComplete, result.Status, "%s: %s", result.Artifact.FileName, result.Err)
			}

			locks := files["my_lock_waits.txt"]
			assert.True(t, blocksWithRows(locks, "data_lock_waits"), "a fast sample sees the row lock waiter")
			assert.True(t, blocksWithRows(locks, "metadata_locks"), "and the ALTER TABLE queued for the metadata lock")
			assert.Contains(t, locks, ",yc_app,orders,", "the pending metadata lock names its table")

			prepared := files["my_prepared_statements.txt"]
			assert.Contains(t, prepared, "WHERE status = '<redacted>' AND customer_id = ?")
			assert.NotContains(t, prepared, preparedLiteral)
			assert.NotContains(t, prepared, ProgramName, "the agent prepares nothing of its own")

			timeouts := files["my_timeouts.txt"]
			lockWaits, executions := timeoutCount(t, timeouts, "1205"), timeoutCount(t, timeouts, "3024")
			if assert.NotEmpty(t, lockWaits) && assert.NotEmpty(t, executions) {
				assert.GreaterOrEqual(t, lockWaits[len(lockWaits)-1]-lockWaits[0], 2, "the row lock and the metadata lock")
				assert.GreaterOrEqual(t, executions[len(executions)-1]-executions[0], 1)
			}

			explain := files["my_explain.txt"]
			assert.Contains(t, explain, "schema_name=yc_app ", "the application statement is planned in its schema")
			assert.Contains(t, explain, `"table_name": "orders"`)
			assert.NotContains(t, explain, "'pending'", "its values are replaced")
			assert.NotContains(t, explain, "customer_id = 7", "numbers too, in 9.x's operation strings")
			assert.NotContains(t, explain, "near '?'", "a prepared statement's sample costs no plan")

			assert.Contains(t, files["my_table_stats.txt"], "\nyc_app,audit_log,,",
				"reads with no index have an empty INDEX_NAME")
		})
	}
}
