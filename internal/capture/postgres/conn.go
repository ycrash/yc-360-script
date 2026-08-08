// Package postgres captures PostgreSQL server state for a yc-360 bundle.
//
// The target database is by hypothesis already in trouble, so every statement is
// read-only and bounded, and the session limits ride in the startup packet
// rather than in SETs after connect.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// ApplicationName tags the agent's session so an operator watching
	// pg_stat_activity can pick it out.
	ApplicationName = "yc-360-postgres-capture"

	// ConnectTimeout bounds TCP connect plus authentication.
	ConnectTimeout = 5 * time.Second

	// StatementTimeout is the client-side per-statement deadline, applied by
	// callers: the backstop for when the server-side statement_timeout does not
	// fire.
	StatementTimeout = 10 * time.Second

	// ModuleDeadline bounds the one-shot metadata capture. It is not the bound
	// for a sampled capture, which derives its own from the configured window -
	// see WindowGrace.
	ModuleDeadline = 30 * time.Second

	// WindowGrace is what a sampled capture adds to its configured window to
	// reach its module deadline. It exists for the final sample, which starts
	// at t0+window: two statements at StatementTimeout each, plus room to write
	// the closing block. Window.Run arms the deadline after connecting so that
	// budget is not spent before sampling starts.
	//
	// Direction §5 specifies 15s, which is less than a two-statement sample's
	// worst case. Expressed as the arithmetic rather than as 25s so it stays
	// true if StatementTimeout moves.
	WindowGrace = 2*StatementTimeout + 5*time.Second
)

// tooManyConnections is SQLSTATE 53300: the server is at max_connections.
const tooManyConnections = "53300"

// ErrTooManyConnections is distinguished from every other connect failure
// because a database at max_connections is one of the incidents this capture
// exists to diagnose.
var ErrTooManyConnections = errors.New("too_many_connections")

// sessionParams rides in the startup packet rather than in SETs after connect:
// SETs leave a window in which the session has no statement_timeout and is not
// read-only, which on a wedged database is exactly when things hang.
//
// All are PGC_USERSET. idle_session_timeout is pinned to 0 because the sampling
// window holds an idle session across the gap between samples, and a server
// default below the window would drop the connection mid-capture; it also sets
// the package's floor at PostgreSQL 14, where that GUC landed - an unknown name
// in the startup packet is a FATAL, not a warning.
var sessionParams = map[string]string{
	"application_name":                    ApplicationName,
	"default_transaction_read_only":       "on",
	"statement_timeout":                   StatementTimeout.String(),
	"lock_timeout":                        "2s",
	"idle_in_transaction_session_timeout": "5s",
	"idle_session_timeout":                "0",
}

// Target is config.Postgres narrowed to what this package needs, which is what
// keeps config out of this package's imports.
type Target struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string
}

// String renders the target for logs with the password redacted.
func (t Target) String() string {
	password := `""`
	if t.Password != "" {
		password = "<redacted>"
	}

	return fmt.Sprintf(
		"host=%q port=%d database=%q username=%q password=%s sslmode=%s",
		t.Host, t.Port, t.Database, t.Username, password, t.SSLMode,
	)
}

// GoString renders the target for %#v, which ignores String - and
// capture.WrapRun formats a failing task with %#v into an agent log that is
// itself uploaded.
//
// The receiver is a value because fmt can only reach GoString on a nested,
// non-addressable struct field through the value method set.
func (t Target) GoString() string {
	return "postgres.Target{" + t.String() + "}"
}

// Conn is a single pgx connection, not a pool: the capture runs a small fixed
// set of statements in sequence, and adding load to a database in trouble is
// the thing to avoid.
type Conn struct {
	conn *pgx.Conn
}

// Connect opens one connection to t, classifying a refusal for want of
// connection slots as ErrTooManyConnections.
//
// No returned error exposes the password: it never enters the DSN, and pgx
// renders a failed connection as user and database only.
func Connect(ctx context.Context, t Target) (*Conn, error) {
	cfg, err := buildConfig(t)
	if err != nil {
		return nil, err
	}

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, classifyConnectError(err)
	}

	return &Conn{conn: conn}, nil
}

// QueryRow delegates to the underlying connection. The per-statement deadline
// is the caller's: pgx sends the query here but reads the row in Scan, so a
// deadline cancelled at this method's return would break the read.
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.conn.QueryRow(ctx, sql, args...)
}

// Query reads a row set. The per-statement deadline is the caller's, for the
// same reason QueryRow's is: the rows are read after this returns.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.conn.Query(ctx, sql, args...)
}

// Close releases the connection.
func (c *Conn) Close(ctx context.Context) error {
	return c.conn.Close(ctx)
}

// buildConfig turns a Target into a pgx connection config.
func buildConfig(t Target) (*pgx.ConnConfig, error) {
	cfg, err := parseConfigWithoutEnvironment(dsn(t))
	if err != nil {
		return nil, err
	}

	// Assigned even when empty: a password is optional, and leaving the field
	// alone would hand the connection to whatever PGPASSWORD or .pgpass holds.
	cfg.Password = t.Password

	// Replaced wholesale, not merged: pgx can populate this from the DSN, the
	// environment or a service file, and assignment drops all of it without
	// depending on libpqEnv staying current.
	cfg.RuntimeParams = maps.Clone(sessionParams)

	cfg.ConnectTimeout = ConnectTimeout

	return cfg, nil
}

// dsn renders the target in libpq keyword/value form. The password is
// deliberately absent - buildConfig assigns it after parsing - so a parse error
// or a logged DSN cannot expose it.
func dsn(t Target) string {
	pairs := [][2]string{
		{"host", t.Host},
		{"port", strconv.Itoa(t.Port)},
		{"dbname", t.Database},
		{"user", t.Username},
		{"sslmode", t.SSLMode},
		{"application_name", ApplicationName},
	}

	parts := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		parts = append(parts, pair[0]+"="+quoteDSNValue(pair[1]))
	}

	return strings.Join(parts, " ")
}

// quoteDSNValue single quotes v and escapes backslash and single quote. Host,
// database and user are free-form YAML strings, so the parser input cannot be
// assumed tame.
func quoteDSNValue(v string) string {
	var b strings.Builder
	b.Grow(len(v) + 2)

	b.WriteByte('\'')
	for i := 0; i < len(v); i++ {
		if c := v[i]; c == '\\' || c == '\'' {
			b.WriteByte('\\')
		}
		b.WriteByte(v[i])
	}
	b.WriteByte('\'')

	return b.String()
}

// libpqEnv mirrors pgconn's unexported parseEnvSettings map (pgconn/config.go),
// verified complete against pgx v5.10.0 - re-check it when that dependency is
// bumped. Drift is silent, but a missed variable can only influence parsing,
// never inject a GUC: buildConfig replaces RuntimeParams after the parse.
var libpqEnv = []string{
	"PGHOST",
	"PGPORT",
	"PGDATABASE",
	"PGUSER",
	"PGPASSWORD",
	"PGPASSFILE",
	"PGAPPNAME",
	"PGCONNECT_TIMEOUT",
	"PGSSLMODE",
	"PGSSLKEY",
	"PGSSLCERT",
	"PGSSLSNI",
	"PGSSLROOTCERT",
	"PGSSLPASSWORD",
	"PGSSLNEGOTIATION",
	"PGTARGETSESSIONATTRS",
	"PGSERVICE",
	"PGSERVICEFILE",
	"PGTZ",
	"PGOPTIONS",
	"PGMINPROTOCOLVERSION",
	"PGMAXPROTOCOLVERSION",
	"PGCHANNELBINDING",
	"PGREQUIREAUTH",
}

// parseConfigMu serializes the environment window in parseConfigWithoutEnvironment.
var parseConfigMu sync.Mutex

// parseConfigWithoutEnvironment parses dsn with the libpq environment removed,
// so the config file is the only thing that decides what the capture connects
// to.
//
// Cleared for the duration of the parse rather than overridden afterwards,
// because PGSERVICE cannot be beaten any other way: it makes ParseConfig read a
// service file, fail outright when that file is missing, and inject settings the
// DSN does not name. pgx offers no opt-out.
//
// The cost is a process-wide window - the mutex serializes this helper against
// itself, not against the rest of the agent - during which a sibling capture
// that execs a child passes on an environment with the PG* variables missing.
// Accepted: losing the capture entirely to a stray PGSERVICE is worse.
func parseConfigWithoutEnvironment(dsn string) (*pgx.ConnConfig, error) {
	parseConfigMu.Lock()
	defer parseConfigMu.Unlock()

	defer clearEnv(libpqEnv)()

	return pgx.ParseConfig(dsn)
}

// clearEnv unsets each named variable and returns a function that restores the
// ones that were set.
func clearEnv(names []string) (restore func()) {
	saved := make(map[string]string, len(names))

	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			saved[name] = value
			os.Unsetenv(name)
		}
	}

	return func() {
		for name, value := range saved {
			os.Setenv(name, value)
		}
	}
}

// classifyConnectError wraps a 53300 refusal in ErrTooManyConnections and
// returns every other error unchanged.
func classifyConnectError(err error) error {
	if err == nil {
		return nil
	}

	if hasSQLState(err, tooManyConnections) {
		return fmt.Errorf("%w: %w", ErrTooManyConnections, err)
	}

	return err
}

// ConnectErrorText renders a Connect failure for the artifact's connect_error
// row.
//
// A classified failure is written as the bare token the artifact contract pins,
// not as err.Error(): that reads "too_many_connections: failed to connect to
// ..." - close enough to pass a careless eye, and wrong for anything matching
// on it.
func ConnectErrorText(err error, t Target) string {
	switch {
	case err == nil:
		return ""

	case errors.Is(err, ErrTooManyConnections):
		return ErrTooManyConnections.Error()

	default:
		return errorText(err, t.Password)
	}
}

// hasSQLState walks err's tree for a *pgconn.PgError carrying the given
// SQLSTATE.
//
// Not errors.As: pgx joins one error per resolved address, and errors.As stops
// at the first *pgconn.PgError - so on a host with both an A and an AAAA record
// an unrelated rejection from the first address would mask a 53300 from the
// second.
func hasSQLState(err error, sqlState string) bool {
	if err == nil {
		return false
	}

	if pgErr, ok := err.(*pgconn.PgError); ok && pgErr.Code == sqlState {
		return true
	}

	switch unwrapper := err.(type) {
	case interface{ Unwrap() error }:
		return hasSQLState(unwrapper.Unwrap(), sqlState)
	case interface{ Unwrap() []error }:
		for _, joined := range unwrapper.Unwrap() {
			if hasSQLState(joined, sqlState) {
				return true
			}
		}
	}

	return false
}
