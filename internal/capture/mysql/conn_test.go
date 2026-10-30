package mysql

import (
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheSessionIsBoundedBeforeAnyCollectorSeesIt(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "db", Port: 3306, Username: "yc", Password: "p"}, &keyRequestGuard{})
	require.NoError(t, err)

	assert.Equal(t, "5000", cfg.Params["max_execution_time"])
	assert.Equal(t, "1", cfg.Params["transaction_read_only"])
	assert.Equal(t, "2", cfg.Params["lock_wait_timeout"])
	assert.Equal(t, "'+00:00'", cfg.Params["time_zone"])
	assert.Equal(t, "program_name:yCrash-DB-Agent", cfg.ConnectionAttributes)
	assert.True(t, cfg.InterpolateParams, "no statement is ever prepared server-side")
	assert.False(t, cfg.AllowCleartextPasswords)
	assert.False(t, cfg.AllowFallbackToPlaintext, "tls=preferred falls back to plaintext")
	assert.True(t, cfg.TLS.InsecureSkipVerify, "omitted tls: is encrypted and unverified")
	assert.Nil(t, cfg.DialFunc, "the key guard watches plaintext logins only")
}

func TestVerifiedTLSNamesTheServer(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "10.0.0.5", TLS: TLS{Verify: true, ServerName: "db.internal"}}, &keyRequestGuard{})
	require.NoError(t, err)

	assert.False(t, cfg.TLS.InsecureSkipVerify)
	assert.Equal(t, "db.internal", cfg.TLS.ServerName)

	_, err = buildConfig(Target{Host: "db", TLS: TLS{Verify: true, CAFile: "/nonexistent/ca.pem"}}, &keyRequestGuard{})
	assert.ErrorContains(t, err, "tls.caFile")
}

func TestAPlaintextLoginIsGuarded(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "db", TLS: TLS{Disabled: true}}, &keyRequestGuard{})
	require.NoError(t, err)

	assert.Nil(t, cfg.TLS)
	assert.NotNil(t, cfg.DialFunc)
}

type recordingConn struct {
	net.Conn

	written [][]byte
}

func (c *recordingConn) Write(p []byte) (int, error) {
	c.written = append(c.written, append([]byte(nil), p...))

	return len(p), nil
}

func TestTheGuardRefusesOnlyTheLoginsKeyRequest(t *testing.T) {
	for _, request := range []byte{requestPublicKeyCachingSha2, requestPublicKeySha256} {
		guard := &keyRequestGuard{}
		conn := &guardedConn{Conn: &recordingConn{}, guard: guard}

		_, err := conn.Write([]byte{40, 0, 0, 1, 0x8d})
		require.NoError(t, err, "the handshake response")

		_, err = conn.Write([]byte{1, 0, 0, 3, request})
		assert.ErrorIs(t, err, ErrPublicKeyRetrieval)
		assert.True(t, guard.refused.Load())
	}

	guard := &keyRequestGuard{}
	recorder := &recordingConn{}
	conn := &guardedConn{Conn: recorder, guard: guard}

	_, _ = conn.Write([]byte{40, 0, 0, 1, 0x8d})
	_, _ = conn.Write([]byte{9, 0, 0, 0, 0x03, 'S', 'E', 'T', ' ', 'x', '=', '1', ';'})

	_, err := conn.Write([]byte{1, 0, 0, 0, 0x01})
	assert.NoError(t, err, "COM_QUIT after the login carries sha256_password's byte and is not a request")
	assert.False(t, guard.refused.Load())
	assert.Len(t, recorder.written, 3)
}

func TestARefusedKeyRequestSaysToEnableTLS(t *testing.T) {
	guard := &keyRequestGuard{}
	guard.refused.Store(true)

	err := classifyConnectError(errors.New("bad connection"), guard)

	assert.ErrorIs(t, err, ErrPublicKeyRetrieval)
	assert.Contains(t, err.Error(), "Enable TLS for this target")
	assert.Equal(t, "public_key_retrieval_refused", ConnectErrorText(err, Target{}))
}

func TestARefusedSlotIsToldApart(t *testing.T) {
	for _, number := range []uint16{errConCount, errTooManyUserConns, errUserLimit} {
		err := classifyConnectError(fmt.Errorf("connect: %w", serverError(number, "Too many connections")), &keyRequestGuard{})

		assert.ErrorIs(t, err, ErrTooManyConnections, number)
		assert.Equal(t, "too_many_connections", ConnectErrorText(err, Target{}))
	}

	err := classifyConnectError(serverError(1045, "Access denied for user 'yc'"), &keyRequestGuard{})
	assert.NotErrorIs(t, err, ErrTooManyConnections)
}

func TestStatusAndRefusalClasses(t *testing.T) {
	assert.Equal(t, "TIMEOUT", readStatus(serverError(errQueryTimeout, "maximum statement execution time exceeded")))
	assert.Equal(t, "ERROR", readStatus(serverError(1146, "Table doesn't exist")))
	assert.True(t, permissionDenied(serverError(errTableAccess, "SELECT command denied")))
	assert.True(t, permissionDenied(serverError(errSpecificAccess, "you need the PROCESS privilege")))
	assert.False(t, permissionDenied(errors.New("Error 1142")), "a number in the text is not the server's")
}

func TestTheTargetNeverPrintsItsPassword(t *testing.T) {
	target := Target{Host: "db", Port: 3306, Username: "yc", Password: "s3cret"}

	for _, rendered := range []string{fmt.Sprint(target), fmt.Sprintf("%+v", target), fmt.Sprintf("%#v", target)} {
		assert.NotContains(t, rendered, "s3cret")
		assert.Contains(t, rendered, "<redacted>")
	}

	assert.Equal(t, "connect failed for <redacted>", errorText(errors.New("connect failed for s3cret"), "s3cret"))
}

func TestVersionsAndVariants(t *testing.T) {
	assert.Equal(t, Version{8, 0, 46}, parseVersion("8.0.46"))
	assert.Equal(t, Version{8, 4, 3}, parseVersion("8.4.3-3"))
	assert.Equal(t, Version{10, 11, 6}, parseVersion("10.11.6-MariaDB-1:10.11.6+maria~ubu2204"))
	assert.Equal(t, Version{}, parseVersion("garbage"))

	assert.True(t, Version{8, 0, 28}.AtLeast(8, 0, 28), ">= at the release where a change takes effect")
	assert.False(t, Version{8, 0, 27}.AtLeast(8, 0, 28))
	assert.True(t, Version{9, 1, 0}.AtLeast(8, 2, 0))
	assert.False(t, Version{}.AtLeast(8, 0, 0), "an unread version takes the older form")

	assert.Equal(t, VariantMariaDB, variantOf("10.11.6-MariaDB", "mariadb.org binary distribution"))
	assert.Equal(t, VariantPercona, variantOf("8.4.3-3", "Percona Server (GPL), Release 3, Revision 1a2b3c"))
	assert.Equal(t, VariantMySQL, variantOf("8.4.3", "MySQL Community Server - GPL"))
}

func TestTheSessionNeverWaitsLongOnARowLock(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "db"}, &keyRequestGuard{})
	require.NoError(t, err)

	assert.Equal(t, "2", cfg.Params["innodb_lock_wait_timeout"],
		"a row lock is waited on no longer than a metadata lock")
}
