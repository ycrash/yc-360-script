package postgres

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testPassword = "s3cr3t-do-not-log"

func testTarget() Target {
	return Target{
		Host:     "db-prod-01.internal",
		Port:     5432,
		Database: "orders_db",
		Username: "ycrash_monitor",
		Password: testPassword,
		SSLMode:  "require",
	}
}

func TestDSN(t *testing.T) {
	got := dsn(testTarget())

	for _, want := range []string{
		"host='db-prod-01.internal'",
		"port='5432'",
		"dbname='orders_db'",
		"user='ycrash_monitor'",
		"sslmode='require'",
		"application_name='yc-360-postgres-capture'",
	} {
		assert.Contains(t, got, want)
	}

	assert.NotContains(t, got, testPassword, "the password must never enter the DSN")
	assert.NotContains(t, got, "password", "not even as a keyword, so there is nothing to redact")
}

func TestDSNQuoting(t *testing.T) {
	tests := []struct {
		name   string
		target Target
	}{
		{
			name: "space, single quote and backslash",
			target: Target{
				Host:     "db host",
				Port:     5432,
				Database: `orders'db`,
				Username: `DOMAIN\ycrash`,
				SSLMode:  "require",
			},
		},
		{
			name: "ipv6 literal host",
			target: Target{
				Host:     "2001:db8::1",
				Port:     5432,
				Database: "orders_db",
				Username: "ycrash_monitor",
				SSLMode:  "require",
			},
		},
		{
			name: "unix socket directory",
			target: Target{
				Host:     "/var/run/postgresql",
				Port:     5432,
				Database: "orders_db",
				Username: "ycrash_monitor",
				SSLMode:  "require",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := buildConfig(tt.target)
			require.NoError(t, err)

			assert.Equal(t, tt.target.Host, cfg.Host)
			assert.Equal(t, uint16(tt.target.Port), cfg.Port)
			assert.Equal(t, tt.target.Database, cfg.Database)
			assert.Equal(t, tt.target.Username, cfg.User)
		})
	}
}

func TestBuildConfigPassword(t *testing.T) {
	t.Run("assigned from the target", func(t *testing.T) {
		cfg, err := buildConfig(testTarget())
		require.NoError(t, err)

		assert.Equal(t, testPassword, cfg.Password)
	})

	t.Run("empty stays empty despite PGPASSWORD", func(t *testing.T) {
		t.Setenv("PGPASSWORD", "from-environment")

		target := testTarget()
		target.Password = ""

		cfg, err := buildConfig(target)
		require.NoError(t, err)

		assert.Empty(t, cfg.Password)
	})

	t.Run("empty stays empty despite PGPASSFILE", func(t *testing.T) {
		passfile := filepath.Join(t.TempDir(), "pgpass")
		require.NoError(t, os.WriteFile(passfile, []byte("*:*:*:*:from-passfile\n"), 0o600))
		t.Setenv("PGPASSFILE", passfile)

		target := testTarget()
		target.Password = ""

		cfg, err := buildConfig(target)
		require.NoError(t, err)

		assert.Empty(t, cfg.Password)
	})
}

func TestBuildConfigIgnoresEnvironment(t *testing.T) {
	servicefile := filepath.Join(t.TempDir(), "pg_service.conf")
	require.NoError(t, os.WriteFile(servicefile, []byte(
		"[hostile]\n"+
			"host=attacker.internal\n"+
			"port=6432\n"+
			"dbname=other_db\n"+
			"user=someone_else\n"+
			"sslmode=disable\n"+
			"options=-c statement_timeout=0\n",
	), 0o600))

	t.Setenv("PGHOST", "env-host.internal")
	t.Setenv("PGPORT", "6543")
	t.Setenv("PGDATABASE", "env_db")
	t.Setenv("PGUSER", "env_user")
	t.Setenv("PGPASSWORD", "from-environment")
	t.Setenv("PGSSLMODE", "disable")
	t.Setenv("PGAPPNAME", "not-the-agent")
	t.Setenv("PGOPTIONS", "-c statement_timeout=0")
	t.Setenv("PGTZ", "Pacific/Kiritimati")
	t.Setenv("PGSERVICE", "hostile")
	t.Setenv("PGSERVICEFILE", servicefile)

	cfg, err := buildConfig(testTarget())
	require.NoError(t, err)

	assert.Equal(t, "db-prod-01.internal", cfg.Host)
	assert.Equal(t, uint16(5432), cfg.Port)
	assert.Equal(t, "orders_db", cfg.Database)
	assert.Equal(t, "ycrash_monitor", cfg.User)
	assert.Equal(t, testPassword, cfg.Password)
	assert.NotNil(t, cfg.TLSConfig, "sslmode=require from the config file, not sslmode=disable from the environment")

	assert.NotContains(t, cfg.RuntimeParams, "options",
		"PGOPTIONS must not ride in the startup packet the session safety depends on")
	assert.NotContains(t, cfg.RuntimeParams, "timezone",
		"nor anything else the environment offers")
	assert.Equal(t, ApplicationName, cfg.RuntimeParams["application_name"])

	assert.Equal(t, "hostile", os.Getenv("PGSERVICE"), "the environment is restored after the parse")
	assert.Equal(t, "from-environment", os.Getenv("PGPASSWORD"))

	t.Run("unresolvable PGSERVICE", func(t *testing.T) {
		t.Setenv("PGSERVICE", "no-such-service")
		t.Setenv("PGSERVICEFILE", filepath.Join(t.TempDir(), "does-not-exist.conf"))

		cfg, err := buildConfig(testTarget())
		require.NoError(t, err)
		assert.Equal(t, "db-prod-01.internal", cfg.Host)
	})
}

func TestBuildConfigSessionSafety(t *testing.T) {
	cfg, err := buildConfig(testTarget())
	require.NoError(t, err)

	assert.Equal(t, map[string]string{
		"application_name":                    "yc-360-postgres-capture",
		"default_transaction_read_only":       "on",
		"statement_timeout":                   "10s",
		"lock_timeout":                        "2s",
		"idle_in_transaction_session_timeout": "5s",
		"idle_session_timeout":                "0",
	}, cfg.RuntimeParams, "these, and nothing else, ride in the startup packet")

	assert.Equal(t, 5*time.Second, cfg.ConnectTimeout)
}

func TestDefaultSampleBudgetCoversATwoStatementSample(t *testing.T) {
	assert.Equal(t, 2*StatementTimeout, DefaultSampleBudget,
		"the budget must outlast the statements one sample is allowed to run")

	assert.Equal(t, 25*time.Second, DefaultSampleBudget+WindowCloseMargin,
		"which is what a closing tick owned by one default-budget collector costs")
}

func TestClassifyConnectError(t *testing.T) {
	tooMany := &pgconn.PgError{
		Severity: "FATAL",
		Code:     "53300",
		Message:  "sorry, too many clients already",
	}

	noHBAEntry := &pgconn.PgError{
		Severity: "FATAL",
		Code:     "28000",
		Message:  `no pg_hba.conf entry for host "::1"`,
	}

	tests := []struct {
		name                   string
		err                    error
		wantTooManyConnections bool
	}{
		{
			name:                   "nil",
			err:                    nil,
			wantTooManyConnections: false,
		},
		{
			name:                   "53300 bare",
			err:                    tooMany,
			wantTooManyConnections: true,
		},
		{

			name:                   "53300 wrapped and joined",
			err:                    fmt.Errorf("failed to connect: %w", errors.Join(errors.New("first address"), tooMany)),
			wantTooManyConnections: true,
		},
		{

			name:                   "53300 behind another PgError in the join",
			err:                    errors.Join(noHBAEntry, tooMany),
			wantTooManyConnections: true,
		},
		{

			name: "53300 behind another PgError, per-address wrapped",
			err: fmt.Errorf("failed to connect: %w", errors.Join(
				fmt.Errorf("[::1]:5432: %w", noHBAEntry),
				fmt.Errorf("127.0.0.1:5432: %w", tooMany),
			)),
			wantTooManyConnections: true,
		},
		{
			name:                   "several PgErrors, none of them 53300",
			err:                    errors.Join(noHBAEntry, noHBAEntry),
			wantTooManyConnections: false,
		},
		{
			name: "28P01 invalid password",
			err: &pgconn.PgError{
				Severity: "FATAL",
				Code:     "28P01",
				Message:  `password authentication failed for user "ycrash_monitor"`,
			},
			wantTooManyConnections: false,
		},
		{
			name:                   "plain network error",
			err:                    &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
			wantTooManyConnections: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyConnectError(tt.err)

			assert.Equal(t, tt.wantTooManyConnections, errors.Is(got, ErrTooManyConnections))

			if tt.err == nil {
				assert.NoError(t, got)
				return
			}

			assert.ErrorIs(t, got, tt.err, "the original error is always still reachable")
			assert.NotContains(t, got.Error(), testPassword)
		})
	}
}

func TestConnectErrorText(t *testing.T) {
	target := testTarget()

	tooMany := classifyConnectError(&pgconn.PgError{
		Severity: "FATAL",
		Code:     "53300",
		Message:  "sorry, too many clients already",
	})
	require.ErrorIs(t, tooMany, ErrTooManyConnections)
	require.Contains(t, tooMany.Error(), "sorry, too many clients already",
		"the wrapped error carries the driver's text, which is what must not reach the row")

	assert.Empty(t, ConnectErrorText(nil, target), "a connection that succeeded has nothing to say")
	assert.Equal(t, "too_many_connections", ConnectErrorText(tooMany, target))

	refused := errors.New("failed to connect to `host=db-prod-01.internal user=ycrash_monitor " +
		"database=orders_db`: dial error (connection refused)")
	assert.Equal(t, refused.Error(), ConnectErrorText(refused, target))

	leaky := fmt.Errorf("authentication failed with %s\nDETAIL: check the password", testPassword)
	assert.Equal(t, "authentication failed with <redacted> DETAIL: check the password",
		ConnectErrorText(leaky, target))
}

func TestConnectFailureCarriesNoPassword(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	target := testTarget()
	target.Host = "127.0.0.1"
	target.Port = 1

	conn, err := Connect(ctx, target)
	require.Error(t, err)
	assert.Nil(t, conn)

	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		assert.NotContains(t, fmt.Sprintf(verb, err), testPassword,
			"a failed connection leaked the password through %s", verb)
	}
}

func TestTargetRedaction(t *testing.T) {
	target := testTarget()

	type task struct {
		Target Target
		Note   string
	}

	nested := task{Target: target, Note: "capture failed"}

	renderings := []struct {
		operand string
		verb    string
		value   any
	}{
		{"value", "%v", target},
		{"value", "%+v", target},
		{"value", "%s", target},
		{"value", "%q", target},
		{"value", "%#v", target},
		{"pointer", "%#v", &target},
		{"nested field", "%#v", nested},
		{"nested field", "%+v", nested},
	}

	for _, r := range renderings {
		out := fmt.Sprintf(r.verb, r.value)

		assert.NotContains(t, out, testPassword, "%s of a %s leaked the password", r.verb, r.operand)
		assert.Contains(t, out, "<redacted>",
			"%s of a %s should say the password is there but hidden", r.verb, r.operand)
		assert.Contains(t, out, "db-prod-01.internal",
			"%s of a %s should still identify the target", r.verb, r.operand)
	}

	t.Run("empty password renders as empty, not redacted", func(t *testing.T) {
		target := testTarget()
		target.Password = ""

		assert.Contains(t, target.String(), `password=""`)
		assert.NotContains(t, target.String(), "<redacted>")
	})
}

func TestStatementDeadlineSitsAboveTheServerTimeout(t *testing.T) {
	assert.Greater(t, StatementDeadline, StatementTimeout,
		"the client-side deadline must fire after the server's statement_timeout: at "+
			"equal values the client's timer fires first, pgx closes the connection when a "+
			"context expires mid-statement, and one slow statement ends the window as "+
			"connection_lost instead of one error= block")
}
