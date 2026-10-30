package mysql

import (
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTheSessionIsBoundedBeforeAnyCollectorSeesIt(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "db", Port: 3306, Username: "yc", Password: "p"}, sessionVariables, &keyRequestGuard{}, &serverGreeting{})
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
}

func TestMariaDBIsBoundedTheSameUnderItsOwnNames(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "db"}, mariadbSessionVariables, &keyRequestGuard{}, &serverGreeting{})
	require.NoError(t, err)

	assert.Equal(t, "5", cfg.Params["max_statement_time"], "in seconds, not milliseconds")
	assert.Equal(t, "1", cfg.Params["tx_read_only"])
	assert.NotContains(t, cfg.Params, "max_execution_time")
	assert.NotContains(t, cfg.Params, "transaction_read_only")
	assert.Len(t, cfg.Params, len(sessionVariables), "every bound has its MariaDB name")
	assert.Equal(t, "2", cfg.Params["lock_wait_timeout"])
	assert.Equal(t, "5000", sessionVariables["max_execution_time"], "MySQL's own set is untouched")
}

func TestVerifiedTLSNamesTheServer(t *testing.T) {
	cfg, err := buildConfig(Target{Host: "10.0.0.5", TLS: TLS{Verify: true, ServerName: "db.internal"}}, sessionVariables, &keyRequestGuard{}, &serverGreeting{})
	require.NoError(t, err)

	assert.False(t, cfg.TLS.InsecureSkipVerify)
	assert.Equal(t, "db.internal", cfg.TLS.ServerName)

	_, err = buildConfig(Target{Host: "db", TLS: TLS{Verify: true, CAFile: "/nonexistent/ca.pem"}}, sessionVariables, &keyRequestGuard{}, &serverGreeting{})
	assert.ErrorContains(t, err, "tls.caFile")
}

func TestOnlyAPlaintextLoginIsGuarded(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })

	for _, tc := range []struct {
		tls     TLS
		guarded bool
	}{
		{tls: TLS{Disabled: true}, guarded: true},
		{tls: TLS{}, guarded: false},
	} {
		cfg, err := buildConfig(Target{Host: "db", TLS: tc.tls}, sessionVariables, &keyRequestGuard{}, &serverGreeting{})
		require.NoError(t, err)

		conn, err := cfg.DialFunc(t.Context(), "tcp", listener.Addr().String())
		require.NoError(t, err)
		conn.Close()

		_, guarded := conn.(*guardedConn)
		assert.Equal(t, tc.guarded, guarded, "under TLS the guard would see ciphertext")
		assert.Equal(t, tc.tls.Disabled, cfg.TLS == nil)
	}
}

// greetingPacket is the start of a server's first packet: the header, the protocol,
// the version and its terminator, and the first bytes of what follows.
func greetingPacket(version string) []byte {
	payload := append([]byte{greetingProtocol}, version...)
	payload = append(payload, 0, 0x2a, 0, 0, 0)

	return append([]byte{byte(len(payload)), 0, 0, 0}, payload...)
}

// chunkedConn returns no more than one chunk per read, the rest of a chunk on the
// next.
type chunkedConn struct {
	net.Conn

	chunks [][]byte
}

func (c *chunkedConn) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}

	n := copy(p, c.chunks[0])

	c.chunks[0] = c.chunks[0][n:]
	if len(c.chunks[0]) == 0 {
		c.chunks = c.chunks[1:]
	}

	return n, nil
}

func readSocket(t *testing.T, conn net.Conn) []byte {
	t.Helper()

	var got []byte

	buf := make([]byte, 7)

	for {
		n, err := conn.Read(buf)
		got = append(got, buf[:n]...)

		if errors.Is(err, io.EOF) {
			return got
		}

		require.NoError(t, err)
	}
}

func TestTheGreetingNamesTheServer(t *testing.T) {
	// As mariadb:10.11.6 and mysql:8.4 sent them on the matrix.
	const mariadb = "5.5.5-10.11.6-MariaDB-1:10.11.6+maria~ubu2204"

	packet := greetingPacket(mariadb)

	var bytewise [][]byte
	for _, b := range packet {
		bytewise = append(bytewise, []byte{b})
	}

	for name, chunks := range map[string][][]byte{
		"one read":         {packet},
		"a byte at a time": bytewise,
		"then more":        {packet, greetingPacket("8.4.13")},
	} {
		sent := slices.Concat(chunks...)

		greeting := &serverGreeting{}
		got := readSocket(t, watch(&chunkedConn{chunks: chunks}, greeting, nil))

		assert.Equal(t, mariadb, greeting.serverVersion(), name)
		assert.Equal(t, sent, got, "%s: every byte reaches the driver", name)
	}

	assert.Equal(t, VariantMariaDB, variantOf(mariadb, ""))

	greeting := &serverGreeting{}
	readSocket(t, watch(&chunkedConn{chunks: [][]byte{greetingPacket("8.4.13")}}, greeting, nil))
	assert.Equal(t, "8.4.13", greeting.serverVersion())
	assert.Equal(t, VariantMySQL, variantOf(greeting.serverVersion(), ""))
}

func TestAnErrorInPlaceOfTheGreetingNamesNothing(t *testing.T) {
	refusal := append([]byte{0x17, 0, 0, 0, 0xff, 0x10, 0x04}, "Too many connections"...)

	greeting := &serverGreeting{}
	readSocket(t, watch(&chunkedConn{chunks: [][]byte{refusal, greetingPacket("8.4.13")}}, greeting, nil))

	assert.Empty(t, greeting.serverVersion(), "and nothing after it is mistaken for one")
}

func TestAnUnterminatedGreetingIsGivenUpOn(t *testing.T) {
	long := append([]byte{0xff, 0, 0, 0, greetingProtocol}, strings.Repeat("x", 4*maxGreeting)...)

	greeting := &serverGreeting{}
	got := readSocket(t, watch(&chunkedConn{chunks: [][]byte{long}}, greeting, nil))

	assert.Empty(t, greeting.serverVersion())
	assert.Len(t, got, len(long))
	assert.Len(t, greeting.read, maxGreeting, "what is kept is capped")
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
	cfg, err := buildConfig(Target{Host: "db"}, sessionVariables, &keyRequestGuard{}, &serverGreeting{})
	require.NoError(t, err)

	assert.Equal(t, "2", cfg.Params["innodb_lock_wait_timeout"],
		"a row lock is waited on no longer than a metadata lock")
}
