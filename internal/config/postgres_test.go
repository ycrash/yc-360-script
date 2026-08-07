package config

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func withCleanGlobalConfig(t *testing.T) {
	t.Helper()
	saved := GlobalConfig
	t.Cleanup(func() { GlobalConfig = saved })
	GlobalConfig = defaultConfig()
}

func validPostgres() *Postgres {
	return &Postgres{
		Host:     "db-prod-01.internal",
		Port:     5432,
		Database: "orders_db",
		Username: "ycrash_monitor",
		Password: "s3cr3t",
		SSLMode:  "require",
	}
}

func TestPostgresIsConfigured(t *testing.T) {
	var absent *Postgres
	assert.False(t, absent.IsConfigured(), "a nil pointer means no postgres: block was supplied")
	assert.True(t, (&Postgres{}).IsConfigured(), "an allocated zero block is configured")
	assert.True(t, validPostgres().IsConfigured())
}

func TestPostgresValidateNilReceiver(t *testing.T) {
	var absent *Postgres
	warnings, err := absent.Validate()
	assert.NoError(t, err, "no block supplied is not an error")
	assert.Empty(t, warnings)
}

func TestPostgresValidateDefaults(t *testing.T) {
	t.Run("filled when omitted", func(t *testing.T) {
		p := &Postgres{Host: "db-prod-01.internal", Username: "ycrash_monitor"}

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.Equal(t, DefaultPostgresPort, p.Port)
		assert.Equal(t, DefaultPostgresDatabase, p.Database)
		assert.Equal(t, DefaultPostgresSSLMode, p.SSLMode)

		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "postgres.database not set")
		assert.Contains(t, warnings[0], "pg_stat_statements")
	})

	t.Run("explicit values untouched", func(t *testing.T) {
		p := validPostgres()

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings, "a fully specified block has nothing to warn about")

		assert.Equal(t, "db-prod-01.internal", p.Host)
		assert.Equal(t, 5432, p.Port)
		assert.Equal(t, "orders_db", p.Database)
		assert.Equal(t, "ycrash_monitor", p.Username)
		assert.Equal(t, "s3cr3t", p.Password)
		assert.Equal(t, "require", p.SSLMode)
	})
}

func TestPostgresValidateNormalization(t *testing.T) {
	p := &Postgres{
		Host:     "  db-prod-01.internal  ",
		Database: "  orders_db  ",
		Username: "  ycrash_monitor  ",
		SSLMode:  "  REQUIRE  ",
		Password: "  s3cr3t  ",
	}

	_, err := p.Validate()
	require.NoError(t, err)

	assert.Equal(t, "db-prod-01.internal", p.Host)
	assert.Equal(t, "orders_db", p.Database)
	assert.Equal(t, "ycrash_monitor", p.Username)
	assert.Equal(t, "require", p.SSLMode, "sslmode is lowercased as well as trimmed")

	assert.Equal(t, "  s3cr3t  ", p.Password)
}

func TestPostgresValidateEmptyBlock(t *testing.T) {
	const wantMsg = "postgres block is present but empty or has no recognised keys " +
		"(valid keys: host, port, database, username, password, sslmode)"

	t.Run("zero block", func(t *testing.T) {
		warnings, err := (&Postgres{}).Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error())

		assert.NotContains(t, err.Error(), "is required")

		assert.Empty(t, warnings)
	})

	t.Run("whitespace-only values are empty", func(t *testing.T) {
		_, err := (&Postgres{Host: "   ", Username: "  "}).Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error())
	})

	t.Run("a single key is not an empty block", func(t *testing.T) {
		_, err := (&Postgres{Port: 5432}).Validate()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "empty or has no recognised keys")
	})
}

func TestPostgresValidateRequiredFields(t *testing.T) {
	tests := []struct {
		name     string
		block    *Postgres
		wantMsgs []string
		notMsgs  []string
	}{
		{
			name:     "missing host",
			block:    &Postgres{Username: "ycrash_monitor"},
			wantMsgs: []string{"postgres.host is required"},
			notMsgs:  []string{"postgres.username is required"},
		},
		{
			name:     "missing username",
			block:    &Postgres{Host: "db-prod-01.internal"},
			wantMsgs: []string{"postgres.username is required"},
			notMsgs:  []string{"postgres.host is required"},
		},
		{
			name:     "both missing",
			block:    &Postgres{Port: 5432},
			wantMsgs: []string{"postgres.host is required", "postgres.username is required"},
		},
		{
			name:     "no cascade from defaulted keys",
			block:    &Postgres{Host: "db-prod-01.internal"},
			wantMsgs: []string{"postgres.username is required"},
			notMsgs:  []string{"postgres.port", "postgres.sslmode"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.block.Validate()
			require.Error(t, err)
			for _, msg := range tt.wantMsgs {
				assert.Contains(t, err.Error(), msg)
			}
			for _, msg := range tt.notMsgs {
				assert.NotContains(t, err.Error(), msg)
			}
		})
	}
}

func TestPostgresValidatePortRange(t *testing.T) {
	tests := []struct {
		name     string
		port     int
		wantPort int
		wantErr  string
	}{
		{name: "zero takes the default", port: 0, wantPort: DefaultPostgresPort},
		{name: "lower bound", port: 1, wantPort: 1},
		{name: "upper bound", port: 65535, wantPort: 65535},
		{name: "negative", port: -1, wantPort: -1, wantErr: "postgres.port -1 is out of range (1-65535)"},
		{name: "above range", port: 70000, wantPort: 70000, wantErr: "postgres.port 70000 is out of range (1-65535)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPostgres()
			p.Port = tt.port

			_, err := p.Validate()
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			}
			assert.Equal(t, tt.wantPort, p.Port)
		})
	}
}

func TestPostgresValidateSSLMode(t *testing.T) {
	t.Run("all six libpq modes are accepted", func(t *testing.T) {
		for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"} {
			p := validPostgres()
			p.SSLMode = mode

			_, err := p.Validate()
			assert.NoError(t, err, "mode %q should be accepted", mode)
			assert.Equal(t, mode, p.SSLMode)
		}
	})

	t.Run("plaintext-capable modes warn and name the mode", func(t *testing.T) {
		tests := []struct {
			mode      string
			certainty string
		}{
			{mode: "disable", certainty: "will not be"}, // does not negotiate; the outcome is certain
			{mode: "allow", certainty: "may not be"},    // negotiates; depends on the server
			{mode: "prefer", certainty: "may not be"},   // likewise
		}

		for _, tt := range tests {
			t.Run(tt.mode, func(t *testing.T) {
				p := validPostgres()
				p.SSLMode = tt.mode

				warnings, err := p.Validate()
				require.NoError(t, err)
				require.Len(t, warnings, 1, "mode %q should warn", tt.mode)
				assert.Contains(t, warnings[0], "postgres.sslmode="+tt.mode,
					"the warning names the mode actually configured")
				assert.Contains(t, warnings[0], tt.certainty)
			})
		}
	})

	t.Run("encrypted modes do not warn", func(t *testing.T) {
		for _, mode := range []string{"require", "verify-ca", "verify-full"} {
			p := validPostgres()
			p.SSLMode = mode

			warnings, err := p.Validate()
			require.NoError(t, err)
			assert.Empty(t, warnings, "mode %q should not warn", mode)
		}
	})

	t.Run("unknown mode is rejected with the valid set", func(t *testing.T) {
		p := validPostgres()
		p.SSLMode = "verify-fully"

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			`postgres.sslmode "verify-fully" is invalid `+
				`(valid values: disable, allow, prefer, require, verify-ca, verify-full)`,
			err.Error())
	})

	t.Run("case and whitespace are normalized before the membership check", func(t *testing.T) {
		p := validPostgres()
		p.SSLMode = " Verify-Full "

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "verify-full", p.SSLMode)
	})
}

func requireUnsetEnv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "placeholder-to-register-cleanup")
	require.NoError(t, os.Unsetenv(name))
}

func TestPostgresValidatePasswordExpansion(t *testing.T) {
	t.Run("whole value", func(t *testing.T) {
		t.Setenv("PG_TEST_PASSWORD", "s3cr3t")

		p := validPostgres()
		p.Password = "${PG_TEST_PASSWORD}"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t", p.Password)
	})

	t.Run("embedded in a larger value", func(t *testing.T) {
		t.Setenv("PG_TEST_PASSWORD", "s3cr3t")

		p := validPostgres()
		p.Password = "pre-${PG_TEST_PASSWORD}-post"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "pre-s3cr3t-post", p.Password)
	})

	t.Run("two references in one value", func(t *testing.T) {
		t.Setenv("PG_TEST_USER_PART", "alpha")
		t.Setenv("PG_TEST_SECRET_PART", "beta")

		p := validPostgres()
		p.Password = "${PG_TEST_USER_PART}:${PG_TEST_SECRET_PART}"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "alpha:beta", p.Password)
	})

	t.Run("unset variable is an error naming it", func(t *testing.T) {
		requireUnsetEnv(t, "PG_YCRASH_PASSWORD")

		p := validPostgres()
		p.Password = "${PG_YCRASH_PASSWORD}"

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			"postgres.password references ${PG_YCRASH_PASSWORD}, which is not set in the environment",
			err.Error())

		assert.Equal(t, "${PG_YCRASH_PASSWORD}", p.Password)
	})

	t.Run("set-but-empty variable is an error naming it", func(t *testing.T) {
		t.Setenv("PG_YCRASH_PASSWORD", "")

		p := validPostgres()
		p.Password = "${PG_YCRASH_PASSWORD}"

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			"postgres.password references ${PG_YCRASH_PASSWORD}, which is set but empty",
			err.Error())
	})

	t.Run("a repeated bad reference is reported once", func(t *testing.T) {
		requireUnsetEnv(t, "PG_TEST_MISSING")

		p := validPostgres()
		p.Password = "${PG_TEST_MISSING}-${PG_TEST_MISSING}"

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			"postgres.password references ${PG_TEST_MISSING}, which is not set in the environment",
			err.Error(), "one mistake with one fix should produce one line")
	})

	t.Run("bare $NAME is not a reference", func(t *testing.T) {
		t.Setenv("PG_TEST_PASSWORD", "SHOULD_NOT_APPEAR")

		p := validPostgres()
		p.Password = "s3cr3t$PG_TEST_PASSWORD"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t$PG_TEST_PASSWORD", p.Password)
	})

	t.Run("a literal dollar survives", func(t *testing.T) {
		p := validPostgres()
		p.Password = "s3cr3t$dollar"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "s3cr3t$dollar", p.Password)
	})

	t.Run("an unclosed brace is not a reference", func(t *testing.T) {
		p := validPostgres()
		p.Password = "s3cr3t${oops"

		_, err := p.Validate()
		require.NoError(t, err, "an unterminated reference is just a password containing '${'")
		assert.Equal(t, "s3cr3t${oops", p.Password)
	})

	t.Run("replacements are never rescanned", func(t *testing.T) {
		t.Setenv("PG_TEST_PASSWORD", "a${PG_TEST_INNER}c")
		t.Setenv("PG_TEST_INNER", "SHOULD_NOT_APPEAR")

		p := validPostgres()
		p.Password = "${PG_TEST_PASSWORD}"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "a${PG_TEST_INNER}c", p.Password)
		assert.NotContains(t, p.Password, "SHOULD_NOT_APPEAR")
	})

	t.Run("no rescan even when the inner name is unset", func(t *testing.T) {
		requireUnsetEnv(t, "PG_TEST_INNER")
		t.Setenv("PG_TEST_PASSWORD", "a${PG_TEST_INNER}c")

		p := validPostgres()
		p.Password = "${PG_TEST_PASSWORD}"

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "a${PG_TEST_INNER}c", p.Password)
	})

	t.Run("a password with no reference is untouched", func(t *testing.T) {
		p := validPostgres()
		p.Password = "  plain s3cr3t  "

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, "  plain s3cr3t  ", p.Password, "and still not trimmed")
	})

	t.Run("an omitted password expands to nothing and is not an error", func(t *testing.T) {
		p := validPostgres()
		p.Password = ""

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Equal(t, "", p.Password)
	})

	t.Run("expansion errors surface alongside required-field errors", func(t *testing.T) {
		requireUnsetEnv(t, "PG_YCRASH_PASSWORD")

		p := &Postgres{Port: 5432, Password: "${PG_YCRASH_PASSWORD}"}

		_, err := p.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "references ${PG_YCRASH_PASSWORD}")
		assert.Contains(t, err.Error(), "postgres.host is required")
		assert.Contains(t, err.Error(), "postgres.username is required")
	})

	t.Run("a block whose only key is a broken password is not an empty block", func(t *testing.T) {
		requireUnsetEnv(t, "PG_YCRASH_PASSWORD")

		p := &Postgres{Password: "${PG_YCRASH_PASSWORD}"}

		_, err := p.Validate()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "empty or has no recognised keys")
		assert.Contains(t, err.Error(), "references ${PG_YCRASH_PASSWORD}")
	})
}

func TestPostgresString(t *testing.T) {
	t.Run("renders every field with the password redacted", func(t *testing.T) {
		got := validPostgres().String()

		assert.Equal(t,
			`host="db-prod-01.internal" port=5432 database="orders_db" `+
				`username="ycrash_monitor" password=<redacted> sslmode=require`,
			got)
		assert.NotContains(t, got, "s3cr3t")
	})

	t.Run("free-form values are quoted so a comma cannot read as a separator", func(t *testing.T) {
		p := validPostgres()
		p.Database = "orders,archive"

		assert.Contains(t, p.String(), `database="orders,archive"`)
	})

	t.Run("an empty password is not redacted", func(t *testing.T) {
		p := validPostgres()
		p.Password = ""

		got := p.String()
		assert.Contains(t, got, `password=""`)
		assert.NotContains(t, got, "<redacted>")
	})

	t.Run("nil receiver is safe", func(t *testing.T) {
		var absent *Postgres
		assert.Equal(t, "<nil>", absent.String())
	})

	t.Run("fmt verbs route through String", func(t *testing.T) {
		p := validPostgres()

		assert.NotContains(t, fmt.Sprintf("%v", p), "s3cr3t")
		//nolint:staticcheck // S1025: the %s verb routing through String() is the property under test.
		assert.NotContains(t, fmt.Sprintf("%s", p), "s3cr3t")
		assert.Contains(t, fmt.Sprintf("%v", p), "password=<redacted>")

		// Reached recursively, as a field of a surrounding struct.
		wrapper := struct{ Postgres *Postgres }{Postgres: p}
		assert.NotContains(t, fmt.Sprintf("%v", wrapper), "s3cr3t")
	})
}

// The verb String cannot cover: %#v ignores Stringer, and WrapRun formats a
// failing task with it into an agent log that is itself uploaded.
func TestPostgresGoString(t *testing.T) {
	t.Run("%#v redacts, through the pointer and through the value", func(t *testing.T) {
		p := validPostgres()

		for _, operand := range []any{p, *p} {
			got := fmt.Sprintf("%#v", operand)

			assert.NotContains(t, got, "s3cr3t")
			assert.Contains(t, got, "password=<redacted>")
			assert.Contains(t, got, `host="db-prod-01.internal"`,
				"redacting must not cost the fields that make the log useful")
		}
	})

	t.Run("reached as a struct field, which is the shape WrapRun formats", func(t *testing.T) {
		p := validPostgres()

		// Both field kinds: a pointer, which is how every holder in the tree
		// carries it, and a value - the non-addressable case that a
		// pointer-receiver GoString could not have been called on.
		pointerField := struct{ Target *Postgres }{Target: p}
		valueField := struct{ Target Postgres }{Target: *p}

		assert.NotContains(t, fmt.Sprintf("%#v", pointerField), "s3cr3t")
		assert.NotContains(t, fmt.Sprintf("%#v", valueField), "s3cr3t")
	})

	t.Run("the pointer is safe under every verb a log line uses", func(t *testing.T) {
		p := validPostgres()

		for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
			assert.NotContains(t, fmt.Sprintf(verb, p), "s3cr3t",
				"the block leaked the password through %s", verb)
		}
	})

	t.Run("an empty password is not redacted", func(t *testing.T) {
		p := validPostgres()
		p.Password = ""

		got := fmt.Sprintf("%#v", p)
		assert.Contains(t, got, `password=""`)
		assert.NotContains(t, got, "<redacted>")
	})

	t.Run("a nil pointer renders <nil> rather than panicking", func(t *testing.T) {
		var absent *Postgres

		// fmt recovers the dereference a value receiver cannot guard against,
		// and prints the same answer String gives for a nil receiver.
		assert.Equal(t, "<nil>", fmt.Sprintf("%#v", absent))
	})
}

func decodeConfig(t *testing.T, doc string) Config {
	t.Helper()
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &c))
	return c
}

// TestPostgresYAMLShapes pins how each way of writing (or mis-writing) the block
// decodes.
func TestPostgresYAMLShapes(t *testing.T) {
	tests := []struct {
		name         string
		doc          string
		wantNil      bool
		wantZero     bool
		wantAssert   func(t *testing.T, p *Postgres)
		wantDescribe string
	}{
		{
			name:         "key absent",
			doc:          "version: \"1\"\noptions:\n  k: test-key\n",
			wantNil:      true,
			wantDescribe: "no postgres: block at all — the case for every existing customer",
		},
		{
			name:         "bare key with no value",
			doc:          "version: \"1\"\noptions:\n  postgres:\n",
			wantNil:      true,
			wantDescribe: "yaml.v3 short-circuits a null before allocating, so this reads as absent. Accepted residual: detecting it would need a custom unmarshal on Config itself, for a shape nobody writes deliberately",
		},
		{
			name:         "empty mapping",
			doc:          "version: \"1\"\noptions:\n  postgres: {}\n",
			wantZero:     true,
			wantDescribe: "allocated, so it is configured and must fail validation rather than silently skip the capture",
		},
		{
			name:         "only unrecognised keys",
			doc:          "version: \"1\"\noptions:\n  postgres:\n    hostname: db-prod-01.internal\n    sslMode: disable\n",
			wantZero:     true,
			wantDescribe: "yaml key matching is case-sensitive and lenient, so `hostname` and `sslMode` are both discarded — this is exactly why the empty-block error lists the valid keys",
		},
		{
			name:     "partial block",
			doc:      "version: \"1\"\noptions:\n  postgres:\n    port: 5432\n",
			wantZero: false,
			wantAssert: func(t *testing.T, p *Postgres) {
				assert.Equal(t, 5432, p.Port)
				assert.Empty(t, p.Host)
			},
			wantDescribe: "one recognised key means the block decoded; validation reports what is missing",
		},
		{
			name: "full block",
			doc: "version: \"1\"\noptions:\n  postgres:\n    host: db-prod-01.internal\n    port: 5432\n" +
				"    database: orders_db\n    username: ycrash_monitor\n    password: ${PG_YCRASH_PASSWORD}\n    sslmode: require\n",
			wantAssert: func(t *testing.T, p *Postgres) {
				assert.Equal(t, "db-prod-01.internal", p.Host)
				assert.Equal(t, "${PG_YCRASH_PASSWORD}", p.Password)
			},
			wantDescribe: "the §2.4 sample, nested correctly",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Log(tt.wantDescribe)
			p := decodeConfig(t, tt.doc).Postgres

			if tt.wantNil {
				assert.Nil(t, p)
				assert.False(t, p.IsConfigured())
				return
			}

			require.NotNil(t, p)
			assert.True(t, p.IsConfigured())
			assert.Equal(t, tt.wantZero, p.isZero())

			if tt.wantAssert != nil {
				tt.wantAssert(t, p)
			}
		})
	}
}

// TestPostgresBlockMustBeNestedUnderOptions pins the failure mode that has no
// code-level defence: a block written at the top level of the config file is
// discarded silently, with no error.
func TestPostgresBlockMustBeNestedUnderOptions(t *testing.T) {
	c := decodeConfig(t, "version: \"1\"\noptions:\n  k: test-key\npostgres:\n  host: db-prod-01.internal\n  username: ycrash_monitor\n")

	assert.Nil(t, c.Postgres, "a top-level postgres: key decodes to nothing")
	assert.Equal(t, "test-key", c.ApiKey, "...while the correctly nested options still decode")
}

func TestPostgresFixtureParsesEndToEnd(t *testing.T) {
	withCleanGlobalConfig(t)

	require.NoError(t, ParseFlags([]string{"yc", "-c", "testdata/postgres.yaml"}))

	pg := GlobalConfig.Postgres
	require.NotNil(t, pg, "the fixture's block must decode")

	assert.Equal(t, "db-prod-01.internal", pg.Host)
	assert.Equal(t, 5432, pg.Port)
	assert.Equal(t, "orders_db", pg.Database)
	assert.Equal(t, "ycrash_monitor", pg.Username)
	assert.Equal(t, "require", pg.SSLMode)

	assert.Equal(t, "${PG_YCRASH_PASSWORD}", pg.Password)
}

func TestPostgresIsNotRegisteredAsAFlag(t *testing.T) {
	withCleanGlobalConfig(t)

	flagSet, _ := registerFlags("yc")

	assert.Nil(t, flagSet.Lookup("postgres"),
		"there must be no -postgres flag: credentials never go on the command line")
}

func TestPostgresInEffectiveFlags(t *testing.T) {
	t.Run("omitted when no block is configured", func(t *testing.T) {
		withCleanGlobalConfig(t)
		GlobalConfig.Postgres = nil

		assert.NotContains(t, EffectiveFlags(), "postgres",
			"runs with no block must be completely unchanged")
	})

	t.Run("echoed with the password redacted", func(t *testing.T) {
		withCleanGlobalConfig(t)
		t.Setenv("PG_YCRASH_PASSWORD", "sup3r-s3cr3t")

		GlobalConfig.Postgres = &Postgres{
			Host:     "db-prod-01.internal",
			Database: "orders_db",
			Username: "ycrash_monitor",
			Password: "${PG_YCRASH_PASSWORD}",
		}
		_, err := GlobalConfig.Postgres.Validate()
		require.NoError(t, err)

		flags := EffectiveFlags()

		assert.Contains(t, flags, `postgres: host="db-prod-01.internal"`)
		assert.Contains(t, flags, "password=<redacted>")

		// Neither the secret nor the reference that produced it.
		assert.NotContains(t, flags, "sup3r-s3cr3t")
		assert.NotContains(t, flags, "${PG_YCRASH_PASSWORD}")

		// Because validation runs before the echo, the values shown are the
		// effective ones rather than the literal ones.
		assert.Contains(t, flags, "port=5432")
		assert.Contains(t, flags, "sslmode=require")
	})
}
