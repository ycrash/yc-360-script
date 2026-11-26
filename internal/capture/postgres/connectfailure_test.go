package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingServer refuses a login with sqlState, or declines TLS when it is empty.
func refusingServer(t *testing.T, sqlState string) Target {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		backend := pgproto3.NewBackend(conn, conn)

		startup, err := backend.ReceiveStartupMessage()
		if err != nil {
			return
		}

		if _, ok := startup.(*pgproto3.SSLRequest); ok {
			conn.Write([]byte("N"))

			return
		}

		backend.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: sqlState, Message: "refused by the test"})
		backend.Flush()
	}()

	return Target{
		Host:     "127.0.0.1",
		Port:     listener.Addr().(*net.TCPAddr).Port,
		Database: "orders_db",
		Username: "postg",
		Password: testPassword,
		TLS:      TLS{Disabled: sqlState != ""},
	}
}

func describeRealConnect(t *testing.T, target Target) (string, string) {
	t.Helper()

	conn, err := Connect(context.Background(), target)
	if conn != nil {
		conn.Close(context.Background())
	}

	require.Error(t, err)

	failure := DescribeConnectFailure(err, target)

	return failure.Cause, failure.Fix
}

func TestDescribeConnectFailureReadsTheServersAnswer(t *testing.T) {
	tests := []struct {
		sqlState string
		cause    string
		fix      string
	}{
		{
			sqlState: "28P01",
			cause:    `the server rejected the login of user "postg": the username or the password is wrong`,
			fix:      "Check username and password in the postgres: block",
		},
		{
			sqlState: "28000",
			cause: `the server refused user "postg" on database "orders_db": the user does not exist, may not log in, ` +
				"or pg_hba.conf does not admit it from this machine",
			fix: "Check username and database in the postgres: block, and the server's pg_hba.conf",
		},
		{
			sqlState: "3D000",
			cause:    `database "orders_db" does not exist on the server`,
			fix:      "Check database in the postgres: block",
		},
		{
			sqlState: "42501",
			cause:    `user "postg" may not connect to database "orders_db" (no CONNECT privilege)`,
			fix:      "Check database in the postgres: block, or grant the user CONNECT on it",
		},
		{
			sqlState: "57P03",
			cause:    "the server is starting up, shutting down or recovering, and is not accepting connections yet",
			fix:      "Run the capture again once the database is up",
		},
		{
			sqlState: "53300",
			cause:    "the server has no free connection slot (max_connections, or the role's or the database's CONNECTION LIMIT)",
			fix:      "Free a connection or raise the limit, then run the capture again",
		},
		{
			sqlState: "08P01",
			cause:    "the server refused the connection (SQLSTATE 08P01)",
			fix:      "The server's reason is in the details below",
		},
	}

	for _, tt := range tests {
		t.Run(tt.sqlState, func(t *testing.T) {
			cause, fix := describeRealConnect(t, refusingServer(t, tt.sqlState))
			assert.Equal(t, tt.cause, cause)
			assert.Equal(t, tt.fix, fix)
		})
	}
}

func TestDescribeConnectFailureNamesAServerThatRefusesTLS(t *testing.T) {
	cause, fix := describeRealConnect(t, refusingServer(t, ""))
	assert.Equal(t, "tls is enabled for this target, but the server does not accept encrypted connections", cause)
	assert.Equal(t, "Enable SSL on the server, or set tls: enabled: false in the postgres: block", fix)
}

func TestDescribeConnectFailureNamesAClosedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	cause, fix := describeRealConnect(t, Target{Host: "127.0.0.1", Port: port, Username: "u", TLS: TLS{Disabled: true}})
	assert.Equal(t, "nothing is accepting connections at 127.0.0.1:"+strconv.Itoa(port), cause)
	assert.Equal(t, "Check port and host in the postgres: block, and that the database is running", fix)
}

func TestDescribeConnectFailurePrefersTheServersAnswerToARefusedAddress(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}

	// localhost on a server listening on IPv4 only: ::1 refuses, 127.0.0.1 answers.
	err := fmt.Errorf("failed to connect: %w", errors.Join(refused, &pgconn.PgError{Code: "28P01"}))

	failure := DescribeConnectFailure(err, Target{Host: "localhost", Port: 5432, Username: "postg"})
	assert.Equal(t, `the server rejected the login of user "postg": the username or the password is wrong`, failure.Cause)
}

func TestDescribeConnectFailureFallsBackToTheAddress(t *testing.T) {
	failure := DescribeConnectFailure(errors.New("unexpected EOF"), Target{Host: "db", Port: 8080})
	assert.Equal(t, "the connection to db:8080 failed", failure.Cause)
	assert.Equal(t, "Check host, port and tls in the postgres: block", failure.Fix)
}
