package cli

import (
	"testing"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postgresValidateFixture(pg *config.Postgres) config.Config {
	return config.Config{
		Options: config.Options{
			OnlyCapture:  true, // sidesteps the -s / -k requirement
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
			Password: "${PG_YCRASH_PASSWORD}",
			SSLMode:  "REQUIRE",
		})

		require.NoError(t, validate())

		pg := config.GlobalConfig.Postgres
		require.NotNil(t, pg)
		assert.Equal(t, "db-prod-01.internal", pg.Host, "trimmed")
		assert.Equal(t, config.DefaultPostgresPort, pg.Port, "defaulted")
		assert.Equal(t, "require", pg.SSLMode, "lowercased")
		assert.Equal(t, "sup3r-s3cr3t", pg.Password, "expanded")

		// And the logged form still hides it.
		assert.NotContains(t, pg.String(), "sup3r-s3cr3t")
	})

	t.Run("warnings do not stop the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",
			SSLMode:  "disable",
		})

		require.NoError(t, validate())
		assert.Equal(t, config.DefaultPostgresDatabase, config.GlobalConfig.Postgres.Database)
	})

	t.Run("missing host stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Username: "ycrash_monitor",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
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
			Password: "${PG_YCRASH_PASSWORD}",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})

	t.Run("invalid sslmode stops the run", func(t *testing.T) {
		config.GlobalConfig = postgresValidateFixture(&config.Postgres{
			Host:     "db-prod-01.internal",
			Username: "ycrash_monitor",
			SSLMode:  "verify-fully",
		})

		assert.Equal(t, ErrInvalidArgumentCantContinue, validate())
	})
}
