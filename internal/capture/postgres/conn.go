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
	// callers: the backstop for when the server-side one does not fire.
	// DefaultHealthInterval is this same 10s, so an interval sample that runs to
	// its timeout consumes its whole interval - the timeline can stay level
	// under load but cannot catch up.
	StatementTimeout = 10 * time.Second

	// ModuleDeadline bounds the one-shot metadata capture. A sampled capture
	// derives its own - see Window.moduleDeadline.
	ModuleDeadline = 30 * time.Second

	// DefaultSampleBudget is one sample of an artifact that declares none: two
	// statements at StatementTimeout each.
	DefaultSampleBudget = 2 * StatementTimeout

	// WindowCloseMargin is room to write the closing block after the last sample.
	WindowCloseMargin = 5 * time.Second
)

// tooManyConnections is SQLSTATE 53300: the server is at max_connections.
const tooManyConnections = "53300"

// ErrTooManyConnections is distinguished from every other connect failure
// because a database at max_connections is one of the incidents this capture
// exists to diagnose.
var ErrTooManyConnections = errors.New("too_many_connections")

// sessionParams rides in the startup packet rather than in SETs after connect:
// SETs leave a window in which the session has no statement_timeout and is not
// read-only, which on a wedged database is when things hang.
//
// idle_session_timeout is pinned to 0 because the window holds an idle session
// between samples. It also sets the package's floor at PostgreSQL 14, where that
// GUC landed: an unknown name in the startup packet is a FATAL.
var sessionParams = map[string]string{
	"application_name":                    ApplicationName,
	"default_transaction_read_only":       "on",
	"statement_timeout":                   StatementTimeout.String(),
	"lock_timeout":                        "2s",
	"idle_in_transaction_session_timeout": "5s",
	"idle_session_timeout":                "0",
}

// Target is config.Postgres narrowed to what this package needs, which keeps
// config out of this package's imports.
type Target struct {
	Host     string
	Port     int
	Database string
	Username string
	Password string
	SSLMode  string
}

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

// GoString redacts under %#v, which ignores String - and WrapRun formats a
// failing task with %#v into an agent log that is itself uploaded. Value
// receiver because fmt reaches GoString on a nested, non-addressable struct
// field only through the value method set.
func (t Target) GoString() string {
	return "postgres.Target{" + t.String() + "}"
}

// Conn is a single pgx connection, not a pool.
type Conn struct {
	conn *pgx.Conn
}

// Connect opens one connection to t, classifying a refusal for want of
// connection slots as ErrTooManyConnections. No returned error exposes the
// password: it never enters the DSN.
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

// QueryRow's per-statement deadline is the caller's: the row is read in Scan,
// after this returns.
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.conn.QueryRow(ctx, sql, args...)
}

// Query's deadline is the caller's, for the same reason QueryRow's is.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return c.conn.Query(ctx, sql, args...)
}

func (c *Conn) Close(ctx context.Context) error {
	return c.conn.Close(ctx)
}

func buildConfig(t Target) (*pgx.ConnConfig, error) {
	cfg, err := parseConfigWithoutEnvironment(dsn(t))
	if err != nil {
		return nil, err
	}

	// Assigned even when empty: leaving the field alone would hand the
	// connection to whatever PGPASSWORD or .pgpass holds.
	cfg.Password = t.Password

	// Replaced wholesale, not merged: pgx can populate this from the DSN, the
	// environment or a service file, and assignment drops all of it without
	// depending on libpqEnv staying current.
	cfg.RuntimeParams = maps.Clone(sessionParams)

	cfg.ConnectTimeout = ConnectTimeout

	return cfg, nil
}

// dsn renders the target in libpq keyword/value form. The password is absent -
// buildConfig assigns it after parsing - so a parse error or a logged DSN cannot
// expose it.
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

// quoteDSNValue escapes v for libpq. Host, database and user are free-form YAML.
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

var parseConfigMu sync.Mutex

// parseConfigWithoutEnvironment parses dsn with the libpq environment removed,
// so the config file is the only thing deciding what the capture connects to.
//
// Cleared for the parse rather than overridden afterwards, because PGSERVICE
// cannot be beaten any other way: it makes ParseConfig read a service file, fail
// outright when that file is missing, and inject settings the DSN does not name.
//
// The cost is a process-wide window during which a sibling capture that execs a
// child passes on an environment with the PG* variables missing.
func parseConfigWithoutEnvironment(dsn string) (*pgx.ConnConfig, error) {
	parseConfigMu.Lock()
	defer parseConfigMu.Unlock()

	defer clearEnv(libpqEnv)()

	return pgx.ParseConfig(dsn)
}

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
// row. A classified failure is written as the bare token the contract pins, not
// err.Error(): that reads "too_many_connections: failed to connect to ..." -
// close enough to pass a careless eye, and wrong for anything matching on it.
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

// hasSQLState walks err's tree for a *pgconn.PgError with the given SQLSTATE.
// Not errors.As: pgx joins one error per resolved address and errors.As stops at
// the first *pgconn.PgError, so on a host with both an A and an AAAA record an
// unrelated rejection from the first would mask a 53300 from the second.
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
