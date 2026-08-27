// Package postgres captures PostgreSQL server state for a yc-360 bundle.
// The target is assumed already in trouble, so every statement is read-only
// and bounded, and session limits ride in the startup packet rather than
// SETs after connect.
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
	// ApplicationName tags the session so pg_stat_activity can identify it.
	ApplicationName = "yc-360-postgres-capture"

	// ConnectTimeout bounds TCP connect plus authentication.
	ConnectTimeout = 5 * time.Second

	// StatementTimeout is the client-side per-statement deadline, backstopping
	// the server-side one. Equals DefaultHealthInterval (10s), so a maxed-out
	// sample consumes its whole interval - the timeline can't catch up under load.
	StatementTimeout = 10 * time.Second

	// ModuleDeadline bounds the one-shot metadata capture; a sampled capture
	// derives its own (see Window.moduleDeadline).
	ModuleDeadline = 30 * time.Second

	// DefaultSampleBudget covers one sample of an artifact that declares none:
	// two statements at StatementTimeout each.
	DefaultSampleBudget = 2 * StatementTimeout

	// WindowCloseMargin is room to write the closing block after the last sample.
	WindowCloseMargin = 5 * time.Second
)

// tooManyConnections is SQLSTATE 53300: the server is at max_connections.
const tooManyConnections = "53300"

// ErrTooManyConnections distinguishes a max_connections refusal from every
// other connect failure.
var ErrTooManyConnections = errors.New("too_many_connections")

// Rides in the startup packet, not SETs after connect: SETs leave a window
// with no statement_timeout and not read-only, which on a wedged database is
// when things hang. idle_session_timeout=0 holds the idle session between
// samples, and floors the package at PG14, where that GUC landed (an unknown
// name in the startup packet is FATAL).
var sessionParams = map[string]string{
	"application_name":                    ApplicationName,
	"default_transaction_read_only":       "on",
	"statement_timeout":                   StatementTimeout.String(),
	"lock_timeout":                        "2s",
	"idle_in_transaction_session_timeout": "5s",
	"idle_session_timeout":                "0",
}

// Target is config.Postgres narrowed to what this package needs, keeping
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

// GoString redacts under %#v, which ignores String; WrapRun formats a failing
// task with %#v into an agent log that is itself uploaded.
func (t Target) GoString() string {
	return "postgres.Target{" + t.String() + "}"
}

// Conn is a single pgx connection, not a pool.
type Conn struct {
	conn *pgx.Conn

	// connectDuration is the dial: TCP, TLS and authentication. Measured here
	// because this is the only place that sees both ends of the attempt.
	connectDuration time.Duration
}

// ConnectDuration is what the connection cost - the measurement an operator
// reaches for ping.out expecting, against the endpoint actually reached.
func (c *Conn) ConnectDuration() time.Duration { return c.connectDuration }

// Connect opens one connection to t, classifying a max_connections refusal as
// ErrTooManyConnections. No returned error exposes the password - it never
// enters the DSN.
func Connect(ctx context.Context, t Target) (*Conn, error) {
	cfg, err := buildConfig(t)
	if err != nil {
		return nil, err
	}

	dialedAt := time.Now()

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, classifyConnectError(err)
	}

	return &Conn{conn: conn, connectDuration: time.Since(dialedAt)}, nil
}

// ExecSimple runs sql over PostgreSQL's simple query protocol and returns the first
// column of every row, in order.
//
// pgx's QueryExecModeSimpleProtocol is not this: it substitutes $n client-side, so an
// unbound placeholder fails with "insufficient arguments" before reaching the server.
// EXPLAIN (GENERIC_PLAN) needs the placeholders delivered intact, as psql delivers them.
//
// The simple protocol executes every statement in a batch, so the caller must refuse a
// multi-statement text first. maxBytes bounds what is retained, not what is read: rows
// past the cap are drained and dropped so the statement still completes on the shared
// connection, and the second return says the cut happened.
func (c *Conn) ExecSimple(ctx context.Context, sql string, maxBytes int) ([]string, bool, error) {
	mrr := c.conn.PgConn().Exec(ctx, sql)

	var (
		lines     []string
		held      int
		truncated bool
	)

	for mrr.NextResult() {
		reader := mrr.ResultReader()

		for reader.NextRow() {
			values := reader.Values()
			if len(values) == 0 {
				continue
			}

			if maxBytes > 0 && held >= maxBytes {
				truncated = true

				continue
			}

			line := string(values[0])
			held += len(line) + 1

			lines = append(lines, line)
		}
	}

	// Close drains to the end and returns the first error of the exchange.
	if err := mrr.Close(); err != nil {
		return nil, false, err
	}

	return lines, truncated, nil
}

// QueryRow's per-statement deadline is the caller's: Scan reads the row after
// this returns. Same for Query.
func (c *Conn) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return c.conn.QueryRow(ctx, sql, args...)
}

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

	// Assigned even when empty: otherwise the connection could fall back to
	// PGPASSWORD or .pgpass.
	cfg.Password = t.Password

	// Replaced wholesale, not merged: pgx can populate this from the DSN, env,
	// or a service file - assignment drops all of it without depending on
	// libpqEnv staying current.
	cfg.RuntimeParams = maps.Clone(sessionParams)

	cfg.ConnectTimeout = ConnectTimeout

	return cfg, nil
}

// dsn renders the target in libpq keyword/value form. The password is absent -
// buildConfig assigns it after parsing - so a logged DSN cannot expose it.
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

// Mirrors pgconn's unexported parseEnvSettings map (pgconn/config.go), verified
// against pgx v5.10.0 - re-check on that dependency's bump. Drift is silent,
// but a missed variable can only affect parsing, never inject a GUC:
// buildConfig replaces RuntimeParams after the parse.
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

// parseConfigWithoutEnvironment clears the libpq env for the parse, not just
// overrides after: PGSERVICE can't be beaten any other way, since it makes
// ParseConfig read a service file, fail if that file is missing, and inject
// settings the DSN doesn't name. Cost: a process-wide window where a sibling
// capture that execs a child passes on an environment missing the PG* vars.
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
// row. A classified failure is written as the bare token the contract pins,
// not err.Error(), which would read "too_many_connections: failed to connect
// to ..." - wrong for anything matching on it.
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
// Not errors.As: pgx joins one error per resolved address, and errors.As stops
// at the first PgError - on a host with both an A and AAAA record, an
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
