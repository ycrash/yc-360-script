package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postgresValidateFixture(pg *config.Postgres) config.Config {
	return config.Config{
		Options: config.Options{
			OnlyCapture:  true,
			JavaHomePath: "/usr/lib/jvm/java-11",
			Postgres:     pg,
		},
	}
}

func TestValidatePostgres(t *testing.T) {
	saved := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = saved })

	logger.Init("", 0, 0, "info")

	t.Run("no block leaves the run untouched", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(nil)

		require.NoError(t, validate())
		assert.Nil(t, config.GlobalConfig.Postgres)
	})

	t.Run("valid block passes and is normalized in place", func(t *testing.T) {
		t.Setenv("PG_YCRASH_PASSWORD", "sup3r-s3cr3t")

		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "  db-prod-01.internal  ",
			Database: "orders_db",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
			Password:        "${PG_YCRASH_PASSWORD}",
		})

		require.NoError(t, validate())

		pg := config.GlobalConfig.Postgres
		require.NotNil(t, pg)
		assert.Equal(t, "db-prod-01.internal", pg.Host, "trimmed")
		assert.Equal(t, config.DefaultPostgresPort, pg.Port, "defaulted")
		assert.True(t, pg.TLSEnabled(), "tls defaulted to encrypted")
		assert.False(t, pg.TLSVerified())
		assert.Equal(t, "sup3r-s3cr3t", pg.Password, "expanded")

		assert.NotContains(t, pg.String(), "sup3r-s3cr3t")
	})

	t.Run("warnings do not stop the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
			TLS:             &config.PostgresTLS{Enabled: new(bool)},
		})

		require.NoError(t, validate(), "a plaintext connection is warned about, not refused")
		assert.Equal(t, config.DefaultPostgresDatabase, config.GlobalConfig.Postgres.Database)
	})

	t.Run("missing host stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})

	t.Run("a block without captureDuration stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate(), "there is no default window")
	})

	t.Run("empty block stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})

	t.Run("unresolvable password reference stops the run", func(t *testing.T) {
		t.Setenv("PG_YCRASH_PASSWORD", "")

		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
			Password:        "${PG_YCRASH_PASSWORD}",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})

	t.Run("a config file open to group or others stops the run", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("no owner, group and other bits to check on Windows")
		}

		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
		})
		config.GlobalConfig.ConfigPath = configFileWithMode(t, 0o644)

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate(),
			"whatever the password holds: the file carries the host, port and user name")
	})

	t.Run("an owner-only config file passes", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
		})
		config.GlobalConfig.ConfigPath = configFileWithMode(t, 0o600)

		require.NoError(t, validate())
	})

	t.Run("an application capture's config file keeps whatever mode it has", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(nil)
		config.GlobalConfig.ConfigPath = configFileWithMode(t, 0o644)

		require.NoError(t, validate(), "no postgres block, no check")
	})

	t.Run("sslmode stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
			SSLMode:         "verify-full",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate(),
			"refused with its tls: form, never read as the unverified default")
	})

	t.Run("verification over plaintext stops the run", func(t *testing.T) {
		verify := true

		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",

			CaptureDuration: twoMinutes(),
			TLS:             &config.PostgresTLS{Enabled: new(bool), VerifyServerCertificate: &verify},
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})
}

func twoMinutes() *config.Duration {
	window := config.Duration(2 * time.Minute)

	return &window
}

func configFileWithMode(t *testing.T, mode os.FileMode) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "db.yaml")
	require.NoError(t, os.WriteFile(path, []byte("options:\n"), 0o600))
	require.NoError(t, os.Chmod(path, mode), "explicitly, since WriteFile's mode is masked by the umask")

	return path
}
