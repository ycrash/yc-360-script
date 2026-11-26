package mysql

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"testing"

	gomysql "github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refusingServer answers a login with an ERR packet in place of the greeting.
func refusingServer(t *testing.T, number uint16) Target {
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

		payload := []byte{0xff, 0, 0}
		binary.LittleEndian.PutUint16(payload[1:], number)
		payload = append(payload, "#HY000refused by the test"...)

		packet := []byte{byte(len(payload)), byte(len(payload) >> 8), byte(len(payload) >> 16), 0}
		conn.Write(append(packet, payload...))
	}()

	return Target{
		Host:     "127.0.0.1",
		Port:     listener.Addr().(*net.TCPAddr).Port,
		Database: "orders_db",
		Username: "yc_monitr",
		Password: "s3cr3t-do-not-log",
		TLS:      TLS{Disabled: true},
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
		number uint16
		cause  string
		fix    string
	}{
		{
			number: 1045,
			cause: `the server rejected the login of user "yc_monitr": the username or the password is wrong, ` +
				"or the account does not allow connections from this machine",
			fix: "Check username and password in the mysql: block, and the account's host part ('user'@'host')",
		},
		{
			number: 1044,
			cause:  `database "orders_db" does not exist, or user "yc_monitr" has no access to it`,
			fix:    "Check database in the mysql: block, or grant the user access to it",
		},
		{
			number: 1049,
			cause:  `database "orders_db" does not exist on the server`,
			fix:    "Check database in the mysql: block",
		},
		{
			number: 1130,
			cause:  "the server has no account that allows connections from this machine's address",
			fix:    "Check username in the mysql: block, and the account's host part ('user'@'host')",
		},
		{
			number: 3159,
			cause:  "the server accepts only encrypted connections (require_secure_transport)",
			fix:    "Set tls: enabled: true in the mysql: block",
		},
		{
			number: 1040,
			cause:  "the server has no free connection slot (max_connections, or the account's max_user_connections)",
			fix:    "Free a connection or raise the limit, then run the capture again",
		},
		{
			number: 3118,
			cause:  "the server refused the connection (error 3118)",
			fix:    "The server's reason is in the details below",
		},
	}

	for _, tt := range tests {
		t.Run(strconv.Itoa(int(tt.number)), func(t *testing.T) {
			cause, fix := describeRealConnect(t, refusingServer(t, tt.number))
			assert.Equal(t, tt.cause, cause)
			assert.Equal(t, tt.fix, fix)
		})
	}
}

func TestDescribeConnectFailureNamesAClosedPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	port := listener.Addr().(*net.TCPAddr).Port
	require.NoError(t, listener.Close())

	cause, fix := describeRealConnect(t, Target{Host: "127.0.0.1", Port: port, Username: "u", TLS: TLS{Disabled: true}})
	assert.Equal(t, "nothing is accepting connections at 127.0.0.1:"+strconv.Itoa(port), cause)
	assert.Equal(t, "Check port and host in the mysql: block, and that the database is running", fix)
}

func TestDescribeConnectFailureNamesTheAgentsOwnRefusals(t *testing.T) {
	guard := &keyRequestGuard{}
	guard.refused.Store(true)

	failure := DescribeConnectFailure(classifyConnectError(errors.New("write: refused"), guard), Target{})
	assert.Contains(t, failure.Cause, "the server's RSA public key over a plaintext connection")
	assert.Equal(t, "Set tls: enabled: true in the mysql: block, then check username and password", failure.Fix)

	failure = DescribeConnectFailure(fmt.Errorf("login: %w", gomysql.ErrNoTLS), Target{})
	assert.Equal(t, "tls is enabled for this target, but the server does not accept encrypted connections", failure.Cause)
	assert.Equal(t, "Enable TLS on the server, or set tls: enabled: false in the mysql: block", failure.Fix)
}

func TestDescribeConnectFailureFallsBackToTheAddress(t *testing.T) {
	failure := DescribeConnectFailure(gomysql.ErrInvalidConn, Target{Host: "db", Port: 8080})
	assert.Equal(t, "the connection to db:8080 failed", failure.Cause)
	assert.Equal(t, "Check host, port and tls in the mysql: block", failure.Fix)
}
