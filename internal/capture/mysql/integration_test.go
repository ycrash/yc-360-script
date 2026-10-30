//go:build mysqlintegration

package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

	cfg := gomysql.NewConfig()
	cfg.User, cfg.Passwd = "root", "yc-root-pw"
	cfg.Net, cfg.Addr = "tcp", fmt.Sprintf("127.0.0.1:%d", server.port)
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
