// Package mysql captures MySQL server state for a yc-360 bundle. The target is
// assumed already in trouble, so every statement is read-only and bounded, and
// the session's limits are set as the connection opens, before it is handed to
// any collector.
package mysql

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"yc-agent/internal/capture/dbcore"
)

const (
	// ProgramName tags every connection as a connection attribute, so
	// performance_schema.session_connect_attrs names the agent's own sessions
	// (spec §1.3).
	ProgramName = "yCrash-DB-Agent"

	// statementMarker prefixes every statement the agent sends. query_sample_text and
	// processlist_info keep it, so the agent's statements are recognisable in the
	// files that carry statement text; the digest is unaffected.
	statementMarker = "/* " + ProgramName + " */ "

	// ConnectTimeout bounds TCP connect, TLS and authentication.
	ConnectTimeout = 5 * time.Second

	// StatementTimeout is the server-side max_execution_time, set as the session
	// opens. A SELECT that reaches it fails with error 3024 and the connection stays
	// open. It covers SELECT only: SHOW and EXPLAIN are bounded by StatementDeadline
	// alone. It is also the floor config puts under mysql.frequency (MinFrequency,
	// pinned equal by a test in internal/capture).
	StatementTimeout = 5 * time.Second

	// StatementDeadline is the client-side bound on the same statement, the backstop
	// for a server that never answers and the only bound on SHOW. The driver closes
	// the socket when a context expires mid-statement, which ends the window for that
	// connection, so it sits above StatementTimeout and the server's own error is
	// what arrives for a SELECT.
	StatementDeadline = StatementTimeout + 5*time.Second

	// LockWaitTimeout is lock_wait_timeout, in seconds: how long a statement waits
	// for a metadata lock - behind an ALTER TABLE, for example - before failing.
	LockWaitTimeout = 2

	// DefaultSampleBudget covers one sample of an artifact that declares none:
	// two statements at StatementTimeout each.
	DefaultSampleBudget = 2 * StatementTimeout

	// WindowCloseMargin is room to write the closing block after the last sample.
	WindowCloseMargin = 5 * time.Second

	// ModuleDeadline bounds a one-shot read outside a window.
	ModuleDeadline = 30 * time.Second

	// idleTimeout is the session's wait_timeout, in seconds: longer than any gap
	// between two samples on one connection, which is at most the two-hour window.
	// The server's own default may be far shorter, and a connection it closes for
	// idleness is not reopened.
	idleTimeout = 9000
)

// sessionVariables are set in one SET as the connection opens, before the driver
// returns it, so no statement ever runs without them. time_zone makes TIMESTAMP
// columns read in UTC, as every agent timestamp is.
var sessionVariables = map[string]string{
	"max_execution_time": strconv.FormatInt(StatementTimeout.Milliseconds(), 10),
	"lock_wait_timeout":  strconv.Itoa(LockWaitTimeout),

	// No agent statement is known to wait on a row lock: consistent reads do not,
	// and EXPLAIN of a FOR UPDATE on a locked row returned at once on the matrix.
	// This bounds the unknown at the metadata-lock bound, against the server's
	// 50s default.
	"innodb_lock_wait_timeout": strconv.Itoa(LockWaitTimeout),
	"transaction_read_only":    "1",
	"wait_timeout":             strconv.Itoa(idleTimeout),
	"time_zone":                "'+00:00'",
}

// mariadbSessionVariables are sessionVariables under MariaDB's names, for the second
// login Connect makes when the first one's SET fails on a MariaDB, which has no
// max_execution_time (and 10.11 no transaction_read_only). max_statement_time is
// in seconds and bounds every statement, SHOW and EXPLAIN as well as SELECT.
var mariadbSessionVariables = func() map[string]string {
	variables := maps.Clone(sessionVariables)

	delete(variables, "max_execution_time")
	delete(variables, "transaction_read_only")

	variables["max_statement_time"] = strconv.FormatFloat(StatementTimeout.Seconds(), 'f', -1, 64)
	variables["tx_read_only"] = "1"

	return variables
}()

// MySQL error numbers the capture tells apart.
const (
	errConCount         = 1040 // max_connections
	errTooManyUserConns = 1203 // the account's max_user_connections
	errUserLimit        = 1226 // a resource limit, max_user_connections among them
	errDBAccessDenied   = 1044
	errTableAccess      = 1142
	errColumnAccess     = 1143
	errSpecificAccess   = 1227 // PROCESS, REPLICATION CLIENT and the like
	errQueryInterrupted = 1317
	errUnknownVariable  = 1193
	errQueryTimeout     = 3024 // max_execution_time
	errStatementTimeout = 1969 // MariaDB's max_statement_time
)

// ErrTooManyConnections distinguishes a refusal for want of a connection slot from
// every other connect failure.
var ErrTooManyConnections = errors.New("too_many_connections")

// ErrPublicKeyRetrieval is a plaintext login the agent refused to complete: the
// server asked for full authentication, which over plaintext sends the password
// encrypted with a key the server hands over - a key a man-in-the-middle could hand
// over instead. The agent never asks for it.
var ErrPublicKeyRetrieval = errors.New("public_key_retrieval_refused")

// publicKeyAdvice is what a refused key request tells the operator.
const publicKeyAdvice = "the account authenticates with caching_sha2_password or sha256_password, " +
	"and the server asked for full authentication; over a plaintext connection that needs the " +
	"server's RSA public key, which the agent never requests because a man-in-the-middle could " +
	"supply its own and read the password. Enable TLS for this target (tls: enabled: true). " +
	"A plaintext login can succeed while the server holds the password in its cache - after " +
	"the account's last TLS login, until a restart or FLUSH PRIVILEGES - so this failure comes and goes. " +
	"A wrong password ends here too: the server asks for full authentication whenever the quick check fails"

// Target is config.MySQL narrowed to what this package needs, keeping config out
// of this package's imports.
type Target struct {
	// ID names the target on every sample block, as target_id=.
	ID string

	Host     string
	Port     int
	Database string
	Username string
	Password string

	TLS TLS
}

// id is the target's name, or its address where it has none.
func (t Target) id() string {
	if t.ID != "" {
		return t.ID
	}

	id := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	if t.Database != "" {
		id += "/" + t.Database
	}

	return id
}

func (t Target) address() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

// TLS is how the connection is encrypted and checked. The zero value is encrypted
// and unverified, the config's default when it has no tls: block.
type TLS struct {
	// Disabled connects in plaintext.
	Disabled bool

	// Verify checks the certificate's chain, and that it names ServerName, or Host
	// when that is empty.
	Verify bool

	// CAFile is what the certificate must chain to; empty is the system's trust store.
	CAFile string

	// ServerName is the name checked on the certificate and sent to the server.
	ServerName string
}

// Mode is the driver's tls= the settings amount to. Never "preferred", which
// falls back to plaintext.
func (t TLS) Mode() string {
	switch {
	case t.Disabled:
		return "false"

	case t.Verify:
		return "verify"
	}

	return "skip-verify"
}

// caFileText is target_tls_ca_file: the file verified against, "system" for the
// system's trust store, and empty when nothing is verified.
func (t TLS) caFileText() string {
	switch {
	case !t.Verify || t.Disabled:
		return ""

	case t.CAFile == "":
		return "system"
	}

	return t.CAFile
}

func (t Target) String() string {
	password := `""`
	if t.Password != "" {
		password = "<redacted>"
	}

	return fmt.Sprintf("host=%q port=%d database=%q username=%q password=%s tls=%s",
		t.Host, t.Port, t.Database, t.Username, password, t.TLS.Mode())
}

// GoString redacts under %#v, which ignores String; WrapRun formats a failing task
// with %#v into an agent log that is itself uploaded.
func (t Target) GoString() string {
	return "mysql.Target{" + t.String() + "}"
}

// Rows is the part of *sql.Rows a collector uses: the seam that makes every
// statement testable without a server.
type Rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Querier runs one statement. No arguments: every statement the agent sends is a
// fixed text, so the driver never prepares one server-side.
type Querier interface {
	Query(ctx context.Context, query string) (Rows, error)
}

// Conn is one pinned connection, never reopened.
type Conn struct {
	db   *sql.DB
	conn *sql.Conn

	// connectDuration is the dial: TCP, TLS and authentication.
	connectDuration time.Duration

	loss loss
}

// loss keeps the first error observed once the driver has closed the connection.
// The statement that finds a lost connection gets the network's error; every later
// one gets the driver's "bad connection" or database/sql's "connection is already
// closed", which say nothing about why.
type loss struct {
	mu     sync.Mutex
	closed func() bool
	err    error
}

func (l *loss) note(err error) {
	if err == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.err != nil || l.closed == nil || !l.closed() {
		return
	}

	l.err = err
}

func (l *loss) get() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.err
}

// lossRows reports a row set's terminal error to the connection's loss record.
type lossRows struct {
	*sql.Rows

	loss *loss
}

func (r *lossRows) Next() bool {
	next := r.Rows.Next()
	if !next {
		r.loss.note(r.Err())
	}

	return next
}

func (r *lossRows) Close() error {
	err := r.Rows.Close()
	r.loss.note(r.Err())

	return err
}

// ConnectDuration is what the connection cost.
func (c *Conn) ConnectDuration() time.Duration { return c.connectDuration }

// Connect opens one connection to t. A refusal for want of a slot is
// ErrTooManyConnections; a plaintext login the agent would not complete is
// ErrPublicKeyRetrieval. No returned error exposes the password: it never enters a
// DSN.
//
// MariaDB is out of scope, but a run against one must say what it reached rather
// than fail on a session variable it lacks: the SET that opens the session fails,
// the greeting has already named the server, and the login is made once more with
// MariaDB's names for the same bounds. identify then reads the variant, and
// my_metadata.txt reports it. A refused login is never retried.
func Connect(ctx context.Context, t Target) (*Conn, error) {
	conn, greeting, err := login(ctx, t, sessionVariables)

	if hasErrorNumber(err, errUnknownVariable) && variantOf(greeting, "") == VariantMariaDB {
		conn, _, err = login(ctx, t, mariadbSessionVariables)
	}

	return conn, err
}

// login is one connection attempt, opened with variables. It returns the server
// version the greeting named, read whether or not the login then completed.
func login(ctx context.Context, t Target, variables map[string]string) (*Conn, string, error) {
	guard := &keyRequestGuard{}
	greeting := &serverGreeting{}

	cfg, err := buildConfig(t, variables, guard, greeting)
	if err != nil {
		return nil, "", err
	}

	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		return nil, "", err
	}

	db := sql.OpenDB(connector)

	// One connection, pinned below and never reopened: a new one would restart
	// every delta baseline under the same artifact.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	dialedAt := time.Now()

	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()

		return nil, greeting.serverVersion(), classifyConnectError(err, guard)
	}

	c := &Conn{db: db, conn: conn, connectDuration: time.Since(dialedAt)}
	c.loss.closed = c.Lost

	return c, greeting.serverVersion(), nil
}

// Query sends query with the agent's marker. Its deadline is the caller's: a
// collector bounds each statement with statementContext.
func (c *Conn) Query(ctx context.Context, query string) (Rows, error) {
	rows, err := c.conn.QueryContext(ctx, statementMarker+query)
	if err != nil {
		c.loss.note(err)

		return nil, err
	}

	return &lossRows{Rows: rows, loss: &c.loss}, nil
}

// Close ends the session and the pool behind it.
func (c *Conn) Close(context.Context) error {
	return errors.Join(c.conn.Close(), c.db.Close())
}

// Lost reports whether the driver has closed the connection underneath the window:
// a killed session, a broken socket, or a context that expired mid-statement - the
// driver closes on all three. Read after every sample, it is what tells a lost
// connection from a failed statement, which leaves the connection open.
func (c *Conn) Lost() bool {
	valid := true

	err := c.conn.Raw(func(driverConn any) error {
		if validator, ok := driverConn.(driver.Validator); ok {
			valid = validator.IsValid()
		}

		return nil
	})

	return err != nil || !valid
}

// LossError is the error the driver closed the connection on; nil while it is open.
func (c *Conn) LossError() error { return c.loss.get() }

// statementContext bounds one statement on the client side, at StatementDeadline;
// the server's own max_execution_time is the bound expected to fire for a SELECT.
func statementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, StatementDeadline)
}

// agentStatement reports a statement text the agent itself sent.
func agentStatement(query string) bool {
	return strings.HasPrefix(strings.TrimLeft(query, " \t\r\n"), statementMarker)
}

// silentLogger drops the driver's own log lines, which go to stderr and would
// repeat what the artifact already records.
type silentLogger struct{}

func (silentLogger) Print(...any) {}

func buildConfig(t Target, variables map[string]string, guard *keyRequestGuard, greeting *serverGreeting) (*gomysql.Config, error) {
	cfg := gomysql.NewConfig()

	cfg.User = t.Username
	cfg.Passwd = t.Password
	cfg.Net = "tcp"
	cfg.Addr = t.address()
	cfg.DBName = t.Database
	cfg.Timeout = ConnectTimeout
	cfg.ReadTimeout = StatementDeadline
	cfg.WriteTimeout = StatementDeadline
	cfg.Logger = silentLogger{}

	cfg.ConnectionAttributes = "program_name:" + ProgramName

	// Without it, every statement with an argument would be prepared server-side,
	// and the agent would appear in its own my_prepared_statements.txt.
	cfg.InterpolateParams = true

	// The cleartext plugin sends the password as typed; the old password scheme is
	// broken. Neither is ever wanted.
	cfg.AllowCleartextPasswords = false
	cfg.AllowOldPasswords = false
	cfg.AllowFallbackToPlaintext = false

	cfg.Params = maps.Clone(variables)

	tlsConfig, err := clientTLS(t)
	if err != nil {
		return nil, err
	}

	cfg.TLS = tlsConfig

	// The greeting is read in either mode: it precedes the TLS upgrade. The key
	// guard watches plaintext logins only; under TLS it would see ciphertext.
	if tlsConfig == nil {
		cfg.TLSConfig = "false"
	} else {
		guard = nil
	}

	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer

		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}

		return watch(conn, greeting, guard), nil
	}

	return cfg, nil
}

// watch wraps a freshly dialled socket: the greeting is recorded from its reads and,
// with a guard, the login's writes are guarded.
func watch(conn net.Conn, greeting *serverGreeting, guard *keyRequestGuard) net.Conn {
	conn = &greetingConn{Conn: conn, greeting: greeting}

	if guard != nil {
		conn = &guardedConn{Conn: conn, guard: guard}
	}

	return conn
}

// serverGreeting is the server's version string as its first packet, the
// handshake greeting, gives it. The greeting comes before TLS and before the login,
// so it names the server even when the login or the session's SET then fails.
// MariaDB's begins "5.5.5-" for old clients' sake; variantOf still finds it.
type serverGreeting struct {
	mu      sync.Mutex
	read    []byte
	done    bool
	version string
}

// maxGreeting caps what is kept looking for the version: the packet header, the
// protocol byte, and a version string far longer than any server sends.
const maxGreeting = 4 + 1 + 255

// greetingProtocol is the handshake protocol every supported server speaks. An
// error packet (0xff) in its place - too many connections, a blocked host - has
// no version.
const greetingProtocol = 10

func (g *serverGreeting) observe(p []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.done {
		return
	}

	g.read = append(g.read, p[:min(len(p), maxGreeting-len(g.read))]...)

	if len(g.read) < 5 {
		return
	}

	if g.read[4] != greetingProtocol {
		g.done = true

		return
	}

	if end := bytes.IndexByte(g.read[5:], 0); end >= 0 {
		g.version = string(g.read[5 : 5+end])
		g.done = true

		return
	}

	g.done = len(g.read) == maxGreeting
}

func (g *serverGreeting) serverVersion() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.version
}

// greetingConn passes every read through, showing the bytes to the greeting until
// it has the version.
type greetingConn struct {
	net.Conn

	greeting *serverGreeting
}

func (c *greetingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.greeting.observe(p[:n])

	return n, err
}

// clientTLS is the tls: block as a tls.Config; nil for plaintext.
func clientTLS(t Target) (*tls.Config, error) {
	if t.TLS.Disabled {
		return nil, nil
	}

	if !t.TLS.Verify {
		// Encrypted, and the certificate is not checked: the config's default, and
		// what the server's auto-generated certificate allows.
		return &tls.Config{InsecureSkipVerify: true}, nil
	}

	serverName := t.TLS.ServerName
	if serverName == "" {
		serverName = t.Host
	}

	config := &tls.Config{ServerName: serverName}

	if t.TLS.CAFile != "" {
		pem, err := os.ReadFile(t.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("tls.caFile: %w", err)
		}

		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("tls.caFile %s holds no PEM certificate", t.TLS.CAFile)
		}

		config.RootCAs = pool
	}

	return config, nil
}

// keyRequestGuard refuses the one packet a plaintext login must never send: the
// request for the server's RSA public key. The driver sends it, with no option to
// stop it, whenever caching_sha2_password or sha256_password asks for full
// authentication over a connection without TLS. Only the login is watched: once it
// completes, a one-byte packet is COM_QUIT, which carries the same byte as
// sha256_password's request.
type keyRequestGuard struct {
	refused atomic.Bool
}

// guardedConn watches the login's writes. authenticated flips on the first read
// after the server's OK, which the guard cannot see; it flips instead on the first
// packet past the login's sequence, a command, which always starts a new sequence
// at zero.
type guardedConn struct {
	net.Conn

	guard  *keyRequestGuard
	writes int
	done   bool
}

// Packet bytes: a 3-byte little-endian length, the sequence number, the payload.
const (
	requestPublicKeyCachingSha2 = 0x02
	requestPublicKeySha256      = 0x01
)

func (c *guardedConn) Write(p []byte) (int, error) {
	if !c.done {
		c.writes++

		// The handshake response is the login's first write, sequence 1. A command
		// opens its own exchange at sequence 0, which only happens once logged in.
		if c.writes > 1 && len(p) >= 4 && p[3] == 0 {
			c.done = true
		}
	}

	if !c.done && isPublicKeyRequest(p) {
		c.guard.refused.Store(true)

		return 0, ErrPublicKeyRetrieval
	}

	return c.Conn.Write(p)
}

// isPublicKeyRequest is a one-byte packet carrying either plugin's request.
func isPublicKeyRequest(p []byte) bool {
	if len(p) != 5 || p[0] != 1 || p[1] != 0 || p[2] != 0 {
		return false
	}

	return p[4] == requestPublicKeyCachingSha2 || p[4] == requestPublicKeySha256
}

func classifyConnectError(err error, guard *keyRequestGuard) error {
	if err == nil {
		return nil
	}

	if guard.refused.Load() {
		return fmt.Errorf("%w: %s", ErrPublicKeyRetrieval, publicKeyAdvice)
	}

	if hasErrorNumber(err, errConCount, errTooManyUserConns, errUserLimit) {
		return fmt.Errorf("%w: %w", ErrTooManyConnections, err)
	}

	return err
}

// ConnectErrorText renders a Connect failure for the closing block's connect_error=.
// A classified failure is written as the bare token a reader matches on; the full
// text goes to the agent log.
func ConnectErrorText(err error, t Target) string {
	switch {
	case err == nil:
		return ""

	case errors.Is(err, ErrTooManyConnections):
		return ErrTooManyConnections.Error()

	case errors.Is(err, ErrPublicKeyRetrieval):
		return ErrPublicKeyRetrieval.Error()
	}

	return errorText(err, t.Password)
}

// errorNumber is the server's error number, zero for an error that is not the
// server's.
func errorNumber(err error) uint16 {
	var serverErr *gomysql.MySQLError
	if errors.As(err, &serverErr) {
		return serverErr.Number
	}

	return 0
}

func hasErrorNumber(err error, numbers ...uint16) bool {
	number := errorNumber(err)

	for _, n := range numbers {
		if number != 0 && number == n {
			return true
		}
	}

	return false
}

// permissionDenied reports a refusal for want of a privilege: a table the role may
// not read, or a global privilege it lacks.
func permissionDenied(err error) bool {
	return hasErrorNumber(err, errTableAccess, errColumnAccess, errSpecificAccess, errDBAccessDenied)
}

// readStatus is the status of a block whose read ended in err.
func readStatus(err error) string {
	switch {
	case err == nil:
		return dbcore.StatusOK

	case hasErrorNumber(err, errQueryTimeout, errStatementTimeout, errQueryInterrupted), errors.Is(err, context.DeadlineExceeded):
		return dbcore.StatusTimeout
	}

	return dbcore.StatusError
}

// errorText renders err for an artifact field, password redacted.
func errorText(err error, password string) string {
	return dbcore.RedactError(err, password)
}
