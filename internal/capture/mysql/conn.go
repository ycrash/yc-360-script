// Package mysql captures MySQL server state for a yc-360 bundle. Every statement
// is read-only and bounded; session limits are set before any collector runs.
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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	gomysql "github.com/go-sql-driver/mysql"

	"yc-agent/internal/capture/dbcore"
)

const (
	// ProgramName is the program_name connection attribute (spec §1.3).
	ProgramName = "yCrash-DB-Agent"

	// statementMarker prefixes every agent statement so it is recognisable in
	// statement text; the digest is unaffected.
	statementMarker = "/* " + ProgramName + " */ "

	// ConnectTimeout bounds TCP connect, TLS and authentication.
	ConnectTimeout = 5 * time.Second

	// StatementTimeout is the session's max_execution_time (SELECT only; error 3024,
	// connection kept). Also config's MinFrequency floor, pinned by a test.
	StatementTimeout = 5 * time.Second

	// StatementDeadline is the client-side backstop, and the only bound on SHOW.
	// Expiry closes the socket, so it sits above StatementTimeout.
	StatementDeadline = StatementTimeout + 5*time.Second

	// LockWaitTimeout is lock_wait_timeout, in seconds.
	LockWaitTimeout = 2

	// DefaultSampleBudget is two statements' worth, for an artifact that declares none.
	DefaultSampleBudget = 2 * StatementTimeout

	// WindowCloseMargin leaves room for the closing block.
	WindowCloseMargin = 5 * time.Second

	// ModuleDeadline bounds a one-shot read outside a window.
	ModuleDeadline = 30 * time.Second

	// idleTimeout is wait_timeout, in seconds: above the two-hour window, since a
	// connection closed for idleness is not reopened.
	idleTimeout = 9000
)

// sessionVariables are set before the connection is returned (see
// sessionConnector). time_zone reads TIMESTAMPs in UTC.
var sessionVariables = map[string]string{
	"max_execution_time": strconv.FormatInt(StatementTimeout.Milliseconds(), 10),
	"lock_wait_timeout":  strconv.Itoa(LockWaitTimeout),

	// No agent statement is known to wait on a row lock; this caps the unknown
	// below the 50s default.
	"innodb_lock_wait_timeout": strconv.Itoa(LockWaitTimeout),
	"transaction_read_only":    "1",
	"wait_timeout":             strconv.Itoa(idleTimeout),
	"time_zone":                "'+00:00'",
}

// mariadbSessionVariables are sessionVariables under MariaDB's names, for Connect's
// retry. max_statement_time is in seconds and bounds every statement.
var mariadbSessionVariables = func() map[string]string {
	variables := maps.Clone(sessionVariables)

	delete(variables, "max_execution_time")
	delete(variables, "transaction_read_only")

	variables["max_statement_time"] = strconv.FormatFloat(StatementTimeout.Seconds(), 'f', -1, 64)
	variables["tx_read_only"] = "1"

	return variables
}()

// sessionVariableNames lists the session variables used for variant, sorted.
func sessionVariableNames(variant string) string {
	variables := sessionVariables
	if variant == VariantMariaDB {
		variables = mariadbSessionVariables
	}

	return strings.Join(slices.Sorted(maps.Keys(variables)), ",")
}

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
	errLockWaitTimeout  = 1205 // lock_wait_timeout and innodb_lock_wait_timeout
	errStatementTimeout = 1969 // MariaDB's max_statement_time
)

// ErrTooManyConnections is a refusal for want of a connection slot.
var ErrTooManyConnections = errors.New("too_many_connections")

// ErrPublicKeyRetrieval is a plaintext login refused because it needed the
// server's RSA key, which a man-in-the-middle could supply.
var ErrPublicKeyRetrieval = errors.New("public_key_retrieval_refused")

const publicKeyAdvice = "the account authenticates with caching_sha2_password or sha256_password, " +
	"and the server asked for full authentication; over a plaintext connection that needs the " +
	"server's RSA public key, which the agent never requests because a man-in-the-middle could " +
	"supply its own and read the password. Enable TLS for this target (tls: enabled: true). " +
	"A plaintext login can succeed while the server holds the password in its cache - after " +
	"the account's last TLS login, until a restart or FLUSH PRIVILEGES - so this failure comes and goes. " +
	"A wrong password ends here too: the server asks for full authentication whenever the quick check fails"

// Target is config.MySQL narrowed to what this package needs.
type Target struct {
	// ID is target_id=.
	ID string

	Host     string
	Port     int
	Database string
	Username string
	Password string

	TLS TLS
}

// id is ID, or the address.
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

// TLS settings. The zero value is encrypted and unverified, the config default.
type TLS struct {
	Disabled bool

	// Verify checks the chain and ServerName (or Host).
	Verify bool

	// CAFile is the trust root; empty is the system store.
	CAFile string

	ServerName string
}

// Mode is the driver's tls= value; never "preferred", which falls back to plaintext.
func (t TLS) Mode() string {
	switch {
	case t.Disabled:
		return "false"

	case t.Verify:
		return "verify"
	}

	return "skip-verify"
}

// caFileText is target_tls_ca_file: the file, "system", or empty when unverified.
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

// GoString redacts under %#v, which WrapRun logs and the log is uploaded.
func (t Target) GoString() string {
	return "mysql.Target{" + t.String() + "}"
}

// Rows is the part of *sql.Rows a collector uses; the test seam.
type Rows interface {
	Columns() ([]string, error)
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

// Querier runs one fixed-text statement, so nothing is prepared server-side.
type Querier interface {
	Query(ctx context.Context, query string) (Rows, error)
}

// Conn is one pinned connection, never reopened.
type Conn struct {
	db   *sql.DB
	conn *sql.Conn

	connectDuration time.Duration

	loss  loss
	cause *causeLogger
}

// loss keeps the first error seen after the driver closed the connection; later
// errors are generic "bad connection".
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

// lossRows reports a row set's terminal error to loss.
type lossRows struct {
	*sql.Rows

	loss  *loss
	cause *causeLogger
}

func (r *lossRows) Err() error { return r.cause.explain(r.Rows.Err()) }

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

// ConnectDuration is the dial's TCP, TLS and authentication time.
func (c *Conn) ConnectDuration() time.Duration { return c.connectDuration }

// Connect opens one connection to t, failing with ErrTooManyConnections or
// ErrPublicKeyRetrieval where they apply. The password never enters a DSN.
//
// If the greeting names MariaDB and the session SET fails, it logs in once more
// with MariaDB's variable names. A refused login is never retried.
func Connect(ctx context.Context, t Target) (*Conn, error) {
	conn, greeting, err := login(ctx, t, sessionVariables)

	if hasErrorNumber(err, errUnknownVariable) && variantOf(greeting, "") == VariantMariaDB {
		conn, _, err = login(ctx, t, mariadbSessionVariables)
	}

	return conn, err
}

// login is one attempt; it returns the greeting's version even on failure.
func login(ctx context.Context, t Target, variables map[string]string) (*Conn, string, error) {
	guard := &keyRequestGuard{}
	greeting := &serverGreeting{}
	cause := &causeLogger{}

	cfg, err := buildConfig(t, guard, greeting, cause)
	if err != nil {
		return nil, "", err
	}

	connector, err := gomysql.NewConnector(cfg)
	if err != nil {
		return nil, "", err
	}

	db := sql.OpenDB(sessionConnector{Connector: connector, set: sessionStatement(variables)})

	// One pinned connection: reopening would restart every delta baseline.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	dialedAt := time.Now()

	conn, err := db.Conn(ctx)
	if err != nil {
		db.Close()

		return nil, greeting.serverVersion(), classifyConnectError(cause.explain(err), guard)
	}

	c := &Conn{db: db, conn: conn, connectDuration: time.Since(dialedAt), cause: cause}
	c.loss.closed = c.Lost

	return c, greeting.serverVersion(), nil
}

// sessionStatement is the marked SET, variables in name order so it is one digest.
func sessionStatement(variables map[string]string) string {
	assignments := make([]string, 0, len(variables))
	for _, name := range slices.Sorted(maps.Keys(variables)) {
		assignments = append(assignments, name+" = "+variables[name])
	}

	return statementMarker + "SET " + strings.Join(assignments, ", ")
}

// sessionConnector runs set on each new connection; the driver's own params would
// send it unmarked and unordered. Errors pass through unchanged for Connect.
type sessionConnector struct {
	driver.Connector

	set string
}

func (c sessionConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}

	execer, ok := conn.(driver.ExecerContext)
	if !ok {
		conn.Close()

		return nil, errors.New("mysql driver connection runs no statement")
	}

	if _, err := execer.ExecContext(ctx, c.set, nil); err != nil {
		conn.Close()

		return nil, err
	}

	return conn, nil
}

// Query sends query with the marker; callers bound it with statementContext.
func (c *Conn) Query(ctx context.Context, query string) (Rows, error) {
	rows, err := c.conn.QueryContext(ctx, statementMarker+query)
	if err != nil {
		err = c.cause.explain(err)
		c.loss.note(err)

		return nil, err
	}

	return &lossRows{Rows: rows, loss: &c.loss, cause: c.cause}, nil
}

func (c *Conn) Close(context.Context) error {
	return errors.Join(c.conn.Close(), c.db.Close())
}

// Lost reports whether the driver closed the connection (killed session, broken
// socket, or expired context), telling a loss from a failed statement.
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

// LossError is the error the connection was lost on; nil while open.
func (c *Conn) LossError() error { return c.loss.get() }

// statementContext bounds one statement at StatementDeadline.
func statementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, StatementDeadline)
}

func agentStatement(query string) bool {
	return strings.HasPrefix(strings.TrimLeft(query, " \t\r\n"), statementMarker)
}

// causeLogger keeps the last error the driver logged: on a broken socket the real
// cause goes only to the log. One per login.
type causeLogger struct {
	mu  sync.Mutex
	err error
}

func (l *causeLogger) Print(v ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()

	for _, arg := range v {
		if err, ok := arg.(error); ok {
			l.err = err
		}
	}
}

// explain wraps a closed-connection err with the logged cause.
func (l *causeLogger) explain(err error) error {
	if !errors.Is(err, gomysql.ErrInvalidConn) && !errors.Is(err, driver.ErrBadConn) {
		return err
	}

	l.mu.Lock()
	cause := l.err
	l.mu.Unlock()

	if cause == nil {
		return err
	}

	return fmt.Errorf("%w: %w", err, cause)
}

func buildConfig(t Target, guard *keyRequestGuard, greeting *serverGreeting, cause *causeLogger) (*gomysql.Config, error) {
	cfg := gomysql.NewConfig()

	cfg.User = t.Username
	cfg.Passwd = t.Password
	cfg.Net = "tcp"
	cfg.Addr = t.address()
	cfg.DBName = t.Database
	cfg.Timeout = ConnectTimeout
	cfg.ReadTimeout = StatementDeadline
	cfg.WriteTimeout = StatementDeadline
	cfg.Logger = cause

	cfg.ConnectionAttributes = "program_name:" + ProgramName

	// Keeps the agent out of its own my_prepared_statements.txt.
	cfg.InterpolateParams = true

	// Never cleartext or old-scheme passwords.
	cfg.AllowCleartextPasswords = false
	cfg.AllowOldPasswords = false
	cfg.AllowFallbackToPlaintext = false

	tlsConfig, err := clientTLS(t)
	if err != nil {
		return nil, err
	}

	cfg.TLS = tlsConfig

	// The greeting precedes TLS; the key guard watches plaintext logins only.
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

// watch records the greeting and, with a guard, guards the login's writes.
func watch(conn net.Conn, greeting *serverGreeting, guard *keyRequestGuard) net.Conn {
	conn = &greetingConn{Conn: conn, greeting: greeting}

	if guard != nil {
		conn = &guardedConn{Conn: conn, guard: guard}
	}

	return conn
}

// serverGreeting is the version from the handshake greeting, read before TLS and
// login, so it is known even when they fail.
type serverGreeting struct {
	mu      sync.Mutex
	read    []byte
	done    bool
	version string
}

// maxGreeting is header + protocol byte + a generous version string.
const maxGreeting = 4 + 1 + 255

// greetingProtocol is handshake v10; an error packet (0xff) has no version.
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

// greetingConn shows reads to the greeting.
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
		// The default; works with the server's auto-generated certificate.
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

// keyRequestGuard blocks the plaintext RSA public-key request, which the driver
// sends unconditionally. Only the login is watched: afterwards the same byte is COM_QUIT.
type keyRequestGuard struct {
	refused atomic.Bool
}

// guardedConn watches the login's writes until the first command (sequence 0).
type guardedConn struct {
	net.Conn

	guard  *keyRequestGuard
	writes int
	done   bool
}

// Packet: 3-byte LE length, sequence number, payload.
const (
	requestPublicKeyCachingSha2 = 0x02
	requestPublicKeySha256      = 0x01
)

func (c *guardedConn) Write(p []byte) (int, error) {
	if !c.done {
		c.writes++

		// After the handshake response, sequence 0 means a command: logged in.
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

// isPublicKeyRequest matches either plugin's one-byte request.
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

// ConnectErrorText is connect_error=: a bare token for a classified failure.
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

// errorNumber is the server's error number, or zero.
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

// permissionDenied reports a missing table or global privilege.
func permissionDenied(err error) bool {
	return hasErrorNumber(err, errTableAccess, errColumnAccess, errSpecificAccess, errDBAccessDenied)
}

func readStatus(err error) string {
	switch {
	case err == nil:
		return dbcore.StatusOK

	case hasErrorNumber(err, errQueryTimeout, errStatementTimeout, errQueryInterrupted), errors.Is(err, context.DeadlineExceeded):
		return dbcore.StatusTimeout
	}

	return dbcore.StatusError
}

// errorText renders err with the password redacted.
func errorText(err error, password string) string {
	return dbcore.RedactError(err, password)
}
