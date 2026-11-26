package dbcore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closedPortDialError dials a just-released port.
func closedPortDialError(t *testing.T) (string, error) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	address := listener.Addr().String()
	require.NoError(t, listener.Close())

	conn, err := net.DialTimeout("tcp", address, 5*time.Second)
	if conn != nil {
		conn.Close()
	}

	require.Error(t, err)

	return address, err
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestNetworkFailureNamesAClosedPort(t *testing.T) {
	address, dialErr := closedPortDialError(t)

	failure, ok := NetworkFailure(fmt.Errorf("failed to connect: %w", dialErr), "postgres", address, 5*time.Second)
	require.True(t, ok, "a refused dial is this platform's refusal: %v", dialErr)
	assert.Equal(t, "nothing is accepting connections at "+address, failure.Cause)
	assert.Equal(t, "Check port and host in the postgres: block, and that the database is running", failure.Fix)
}

func TestNetworkFailureTellsTheCausesApart(t *testing.T) {
	const address = "db.example:5433"

	unresolved := &net.DNSError{Err: "no such host", Name: "db.example", IsNotFound: true}
	unverified := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}

	tests := []struct {
		name  string
		err   error
		cause string
		fix   string
	}{
		{
			name:  "a name that does not resolve",
			err:   fmt.Errorf("hostname resolving error: %w", unresolved),
			cause: `the host name "db.example" could not be resolved`,
			fix:   "Check host in the mysql: block",
		},
		{
			name:  "the dial's deadline",
			err:   fmt.Errorf("timeout: %w", context.DeadlineExceeded),
			cause: "db.example:5433 did not answer within 5s",
			fix:   "Check host and port in the mysql: block, and that no firewall blocks the connection",
		},
		{
			name:  "a network timeout",
			err:   &net.OpError{Op: "dial", Net: "tcp", Err: timeoutError{}},
			cause: "db.example:5433 did not answer within 5s",
			fix:   "Check host and port in the mysql: block, and that no firewall blocks the connection",
		},
		{
			name:  "a run stopped while dialling",
			err:   fmt.Errorf("dial: %w", context.Canceled),
			cause: "the run was stopped before the connection opened",
		},
		{
			name:  "a certificate that failed verification",
			err:   fmt.Errorf("tls error: %w", unverified),
			cause: "the server's TLS certificate failed verification",
			fix:   "Check tls.caFile and tls.serverName in the mysql: block",
		},
		{
			name:  "any other network failure",
			err:   &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("network is unreachable")},
			cause: "db.example:5433 could not be reached",
			fix:   "Check host and port in the mysql: block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure, ok := NetworkFailure(tt.err, "mysql", address, 5*time.Second)
			require.True(t, ok)
			assert.Equal(t, tt.cause, failure.Cause)
			assert.Equal(t, tt.fix, failure.Fix)
		})
	}
}

func TestNetworkFailureLeavesTheRestToTheEngine(t *testing.T) {
	for _, err := range []error{nil, errors.New("password authentication failed")} {
		_, ok := NetworkFailure(err, "postgres", "db:5432", 5*time.Second)
		assert.False(t, ok, "%v is the engine's to describe", err)
	}
}

func TestCheckSettingsListsTheKeys(t *testing.T) {
	assert.Equal(t, "Check database in the postgres: block", CheckSettings("postgres", "database"))
	assert.Equal(t, "Check username and password in the postgres: block",
		CheckSettings("postgres", "username", "password"))
	assert.Equal(t, "Check host, port and tls in the mysql: block", CheckSettings("mysql", "host", "port", "tls"))
}
