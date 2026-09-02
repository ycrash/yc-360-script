package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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

// validPostgres is a fully specified block. Frequency is set because the spec's
// 5m default does not fit the default window and Validate says so; a fixture that
// left it unset would carry that warning into every test that counts warnings.
func validPostgres() *Postgres {
	return &Postgres{
		Host:      "db-prod-01.internal",
		Port:      5432,
		Database:  "orders_db",
		Username:  "ycrash_monitor",
		Password:  "s3cr3t",
		SSLMode:   "require",
		Frequency: newDuration(30 * time.Second),
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

		require.Len(t, warnings, 2)
		assert.Contains(t, warnings[0], "postgres.database not set")
		assert.Contains(t, warnings[0], "pg_stat_statements")
		assert.Contains(t, warnings[1], "postgres.frequency is unset and defaults to 5m0s",
			"the spec's default does not fit the default window, and a block that set neither is told")
	})

	t.Run("explicit values untouched", func(t *testing.T) {
		p := validPostgres()

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings, "a fully specified block has nothing to warn about")
		assert.Equal(t, 30*time.Second, p.Frequency.Duration())

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
		"(valid keys: host, port, database, username, password, sslmode, captureDuration, frequency, " +
		"explain, agentOnDbHost)"

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

	t.Run("a block whose only key is captureDuration is an empty block", func(t *testing.T) {
		p := decodePostgresBlock(t, "captureDuration: 90s")

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error(),
			"a duration alone names nothing to capture")
	})

	t.Run("a block whose only key is agentOnDbHost is an empty block", func(t *testing.T) {
		p := decodePostgresBlock(t, "agentOnDbHost: true")

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error(),
			"a declaration about this machine names no database to declare it about")
	})

	t.Run("a block whose only key is frequency is an empty block", func(t *testing.T) {
		p := decodePostgresBlock(t, "frequency: 30s")

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error(),
			"a cadence alone names no database to sample")
	})

	t.Run("a block whose only key is explain is an empty block", func(t *testing.T) {
		p := decodePostgresBlock(t, "explain: all")

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t, wantMsg, err.Error(),
			"a mode alone names no database to explain against; the alternative is an obscure connect failure")
	})
}

func decodePostgresBlock(t *testing.T, body string) *Postgres {
	t.Helper()

	var doc strings.Builder
	doc.WriteString("postgres:\n")
	for line := range strings.SplitSeq(body, "\n") {
		doc.WriteString("  " + line + "\n")
	}

	var block struct {
		Postgres *Postgres `yaml:"postgres"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(doc.String()), &block))
	require.NotNil(t, block.Postgres, "the block itself must decode")

	return block.Postgres
}

func TestPostgresValidateCaptureDuration(t *testing.T) {
	withTarget := func(t *testing.T, body string) *Postgres {
		t.Helper()

		return decodePostgresBlock(t, "host: db-prod-01.internal\n"+
			"database: orders_db\nusername: ycrash_monitor\nsslmode: require\nfrequency: 30s\n"+body)
	}

	t.Run("absent takes the default without warning", func(t *testing.T) {
		p := withTarget(t, "")

		warnings, err := p.Validate()
		require.NoError(t, err)

		require.NotNil(t, p.CaptureDuration)
		assert.Equal(t, DefaultPostgresCaptureDuration, p.CaptureDuration.Duration())

		assert.Empty(t, warnings, "the default is not worth a warning")
	})

	t.Run("an explicit value is kept", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 90s")

		warnings, err := p.Validate()
		require.NoError(t, err)

		require.NotNil(t, p.CaptureDuration)
		assert.Equal(t, 90*time.Second, p.CaptureDuration.Duration())

		assert.Empty(t, warnings)
	})

	t.Run("the ceiling clamps and the warning names both values", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 3h")

		warnings, err := p.Validate()
		require.NoError(t, err, "an over-large window is honoured in part, not rejected")

		require.NotNil(t, p.CaptureDuration)
		assert.Equal(t, MaxPostgresCaptureDuration, p.CaptureDuration.Duration())

		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "3h0m0s", "the warning must name what was asked for")
		assert.Contains(t, warnings[0], "2h0m0s", "the warning must name what was done")
	})

	t.Run("the ceiling is the spec's two-hour performance-test case", func(t *testing.T) {
		assert.Equal(t, 2*time.Hour, MaxPostgresCaptureDuration,
			"raised from 10m for spec v1.2 - the host files stretch with it, by decision")
	})

	t.Run("the ceiling itself is not clamped", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 2h")

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.Equal(t, MaxPostgresCaptureDuration, p.CaptureDuration.Duration())
		assert.Empty(t, warnings, "the boundary is allowed, so it does not warn")
	})

	t.Run("non-positive values are rejected", func(t *testing.T) {
		for _, value := range []string{"0s", "-5s", "0ms"} {
			t.Run(value, func(t *testing.T) {
				p := withTarget(t, "captureDuration: "+value)
				require.NotNil(t, p.CaptureDuration, "an explicit zero decodes to a non-nil pointer")

				_, err := p.Validate()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "postgres.captureDuration")
				assert.Contains(t, err.Error(), "must be positive")
			})
		}
	})

	t.Run("a bare key decodes to nil and is treated as absent", func(t *testing.T) {
		p := withTarget(t, "captureDuration:")
		require.Nil(t, p.CaptureDuration, "an explicit null does not reach UnmarshalYAML")

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, DefaultPostgresCaptureDuration, p.CaptureDuration.Duration())
	})

	t.Run("an unparseable value is the decoder's error, naming the key", func(t *testing.T) {
		var block struct {
			Postgres *Postgres `yaml:"postgres"`
		}
		err := yaml.Unmarshal([]byte("postgres:\n  captureDuration: 2 minutes\n"), &block)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration format")
	})
}

func TestPostgresValidateFrequency(t *testing.T) {
	withTarget := func(t *testing.T, body string) *Postgres {
		t.Helper()

		return decodePostgresBlock(t, "host: db-prod-01.internal\n"+
			"database: orders_db\nusername: ycrash_monitor\nsslmode: require\n"+body)
	}

	t.Run("absent takes the spec's 5m default, which the default window cannot fit", func(t *testing.T) {
		p := withTarget(t, "")

		warnings, err := p.Validate()
		require.NoError(t, err, "the bookend is the spec's safety net, so this is a warning")

		require.NotNil(t, p.Frequency)
		assert.Equal(t, DefaultPostgresFrequency, p.Frequency.Duration())

		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "postgres.frequency is unset and defaults to 5m0s")
		assert.Contains(t, warnings[0], "not shorter than the 2m0s window")
		assert.Contains(t, warnings[0], "only the opening and closing samples")
		assert.Contains(t, warnings[0], "for example 30s",
			"the spec's own incident value, named as the fix")
	})

	t.Run("absent on a window longer than the default takes it without warning", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 2h")

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.Equal(t, DefaultPostgresFrequency, p.Frequency.Duration(),
			"the spec's performance-test case: 2h at 5m, twenty-five samples")
		assert.Empty(t, warnings)
	})

	t.Run("an explicit value is kept", func(t *testing.T) {
		p := withTarget(t, "frequency: 30s")

		warnings, err := p.Validate()
		require.NoError(t, err)

		require.NotNil(t, p.Frequency)
		assert.Equal(t, 30*time.Second, p.Frequency.Duration(),
			"the spec's incident case: 30s on the 2m default window")
		assert.Empty(t, warnings)
	})

	t.Run("the floor clamps and the warning names both values", func(t *testing.T) {
		p := withTarget(t, "frequency: 2s")

		warnings, err := p.Validate()
		require.NoError(t, err, "a too-fast cadence is honoured in part, not rejected")

		require.NotNil(t, p.Frequency)
		assert.Equal(t, MinPostgresFrequency, p.Frequency.Duration())

		require.Len(t, warnings, 1)
		assert.Contains(t, warnings[0], "2s is below", "the warning must name what was asked for")
		assert.Contains(t, warnings[0], "10s", "the warning must name what was done")
	})

	t.Run("the floor itself is not clamped", func(t *testing.T) {
		p := withTarget(t, "frequency: 10s")

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.Equal(t, MinPostgresFrequency, p.Frequency.Duration())
		assert.Empty(t, warnings, "the boundary is allowed, so it does not warn")
	})

	t.Run("a cadence no shorter than the window warns that only the bookend remains", func(t *testing.T) {
		for _, body := range []string{
			"frequency: 5m",
			"captureDuration: 2m\nfrequency: 2m",
			"captureDuration: 3h\nfrequency: 2h",
		} {
			t.Run(body, func(t *testing.T) {
				p := withTarget(t, body)

				warnings, err := p.Validate()
				require.NoError(t, err, "the bookend is the spec's safety net, so this is a warning")

				require.NotEmpty(t, warnings)
				assert.Contains(t, warnings[len(warnings)-1], "only the opening and closing samples")
			})
		}
	})

	t.Run("a cadence shorter than the window does not warn", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 2m\nfrequency: 1m59s")

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings)
	})

	t.Run("non-positive values are rejected", func(t *testing.T) {
		for _, value := range []string{"0s", "-5s", "0ms"} {
			t.Run(value, func(t *testing.T) {
				p := withTarget(t, "frequency: "+value)
				require.NotNil(t, p.Frequency, "an explicit zero decodes to a non-nil pointer")

				_, err := p.Validate()
				require.Error(t, err)
				assert.Contains(t, err.Error(), "postgres.frequency")
				assert.Contains(t, err.Error(), "must be positive")
				assert.Contains(t, err.Error(), "omit the key for the 5m0s default")
			})
		}
	})

	t.Run("a bare key decodes to nil and is treated as absent", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 1h\nfrequency:")
		require.Nil(t, p.Frequency, "an explicit null does not reach UnmarshalYAML")

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, DefaultPostgresFrequency, p.Frequency.Duration())
		assert.Empty(t, warnings)
	})

	t.Run("a rejected window does not also produce a bookend warning", func(t *testing.T) {
		p := withTarget(t, "captureDuration: 0s")

		warnings, err := p.Validate()
		require.Error(t, err)
		assert.Empty(t, warnings, "one fault, one message")
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
			{mode: "disable", certainty: "will not be"},
			{mode: "allow", certainty: "may not be"},
			{mode: "prefer", certainty: "may not be"},
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

func TestPostgresValidateExplain(t *testing.T) {
	withTarget := func(t *testing.T, body string) *Postgres {
		t.Helper()

		return decodePostgresBlock(t, "host: db-prod-01.internal\n"+
			"database: orders_db\nusername: ycrash_monitor\nsslmode: require\nfrequency: 30s\n"+body)
	}

	t.Run("the two accepted values", func(t *testing.T) {
		for _, mode := range []string{ExplainLogged, ExplainAll} {
			p := validPostgres()
			p.Explain = mode

			_, err := p.Validate()
			assert.NoError(t, err, "mode %q should be accepted", mode)
			assert.Equal(t, mode, p.Explain)
		}
	})

	t.Run("omitted is off, and says nothing", func(t *testing.T) {
		p := validPostgres()

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings)
		assert.Empty(t, p.Explain)
		assert.Equal(t, ExplainOff, p.ExplainMode())
	})

	t.Run("an empty value is the same as omitting the key", func(t *testing.T) {
		for _, body := range []string{"explain:", "explain: \"\"", "explain: ~"} {
			p := withTarget(t, body)

			warnings, err := p.Validate()
			require.NoError(t, err, "%q should decode to an unset mode", body)
			assert.Empty(t, warnings)
			assert.Equal(t, ExplainOff, p.ExplainMode())
		}
	})

	t.Run("the boolean a human types is named, not rejected generically", func(t *testing.T) {
		for _, typed := range []string{"true", "false", "on", "off", "yes", "no"} {
			p := withTarget(t, "explain: "+typed)

			_, err := p.Validate()
			require.Error(t, err, "%q should be rejected", typed)
			assert.Equal(t,
				`postgres.explain is "`+typed+`" - it takes "logged" or "all"; `+
					`omit the key to capture no plans`,
				err.Error())
		}
	})

	t.Run("True is lowercased before the boolean check, not after it", func(t *testing.T) {
		p := withTarget(t, "explain: True")
		require.Equal(t, "True", p.Explain, "yaml.v3 delivers it capitalised")

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			`postgres.explain is "true" - it takes "logged" or "all"; `+
				`omit the key to capture no plans`,
			err.Error(),
			"lowercasing first is what keeps this out of the generic invalid-value path")
	})

	t.Run("an unknown value is rejected with the valid set", func(t *testing.T) {
		p := validPostgres()
		p.Explain = "estimated"

		_, err := p.Validate()
		require.Error(t, err)
		assert.Equal(t,
			`postgres.explain "estimated" is invalid (valid values: logged, all)`,
			err.Error())
	})

	t.Run("case and whitespace are normalized before the membership check", func(t *testing.T) {
		p := validPostgres()
		p.Explain = "  LOGGED  "

		_, err := p.Validate()
		require.NoError(t, err)
		assert.Equal(t, ExplainLogged, p.Explain)
	})

	t.Run("all warns once, and says what leaves and what lands", func(t *testing.T) {
		p := validPostgres()
		p.Explain = ExplainAll

		warnings, err := p.Validate()
		require.NoError(t, err)
		require.Len(t, warnings, 1, "a fully specified block has nothing else to warn about")
		assert.Contains(t, warnings[0], "postgres.explain=all")
		assert.Contains(t, warnings[0], "submitted back to the database as EXPLAIN")
		assert.Contains(t, warnings[0], "literal parameter values from your data")
	})

	t.Run("logged does not warn: nothing is submitted", func(t *testing.T) {
		p := validPostgres()
		p.Explain = ExplainLogged

		warnings, err := p.Validate()
		require.NoError(t, err)
		assert.Empty(t, warnings)
	})
}

func TestPostgresExplainMode(t *testing.T) {
	var absent *Postgres
	assert.Equal(t, ExplainOff, absent.ExplainMode(), "nil block is a run with no plans")
	assert.Equal(t, ExplainOff, (&Postgres{}).ExplainMode())
	assert.Equal(t, ExplainAll, (&Postgres{Explain: ExplainAll}).ExplainMode())

	assert.NotContains(t, postgresExplainModes, ExplainOff,
		`"off" reports the omitted key; accepting it as input would be a second way to say one thing`)
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
		p := validPostgres()
		window := Duration(90 * time.Second)
		p.CaptureDuration = &window

		got := p.String()

		assert.Equal(t,
			`host="db-prod-01.internal" port=5432 database="orders_db" `+
				`username="ycrash_monitor" password=<redacted> sslmode=require `+
				`captureDuration=1m30s frequency=30s explain=off agentOnDbHost=false`,
			got)
		assert.NotContains(t, got, "s3cr3t")
	})

	t.Run("an omitted mode reads as off rather than blank", func(t *testing.T) {
		assert.Contains(t, validPostgres().String(), "explain=off")
	})

	t.Run("a configured mode is echoed", func(t *testing.T) {
		p := validPostgres()
		p.Explain = ExplainAll

		assert.Contains(t, p.String(), "explain=all")
	})

	t.Run("an unset window says so rather than reading as a value", func(t *testing.T) {
		got := validPostgres().String()

		assert.Contains(t, got, "captureDuration=(unset, defaults to 2m0s)")
	})

	t.Run("an unset cadence says so rather than reading as a value", func(t *testing.T) {
		p := validPostgres()
		p.Frequency = nil

		assert.Contains(t, p.String(), "frequency=(unset, defaults to 5m0s)")
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

		assert.NotContains(t, fmt.Sprintf("postgres: %s", p), "s3cr3t")
		assert.Contains(t, fmt.Sprintf("%v", p), "password=<redacted>")

		wrapper := struct{ Postgres *Postgres }{Postgres: p}
		assert.NotContains(t, fmt.Sprintf("%v", wrapper), "s3cr3t")
	})
}

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

		assert.Equal(t, "<nil>", fmt.Sprintf("%#v", absent))
	})
}

func decodeConfig(t *testing.T, doc string) Config {
	t.Helper()
	var c Config
	require.NoError(t, yaml.Unmarshal([]byte(doc), &c))
	return c
}

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
			wantDescribe: "every recognised key, nested correctly",
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

		assert.NotContains(t, flags, "sup3r-s3cr3t")
		assert.NotContains(t, flags, "${PG_YCRASH_PASSWORD}")

		assert.Contains(t, flags, "port=5432")
		assert.Contains(t, flags, "sslmode=require")
		assert.Contains(t, flags, "explain=off",
			"the run's plan-capture intent belongs in the echo; it is not a credential")
	})
}

func TestPostgresAgentOnDBHost(t *testing.T) {
	t.Run("omitted is the default and says nothing", func(t *testing.T) {
		p := validPostgres()

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.False(t, p.AgentOnDBHost)
		assert.Empty(t, warnings)
	})

	t.Run("a declaration is decoded and warned about", func(t *testing.T) {
		p := decodePostgresBlock(t, `
host: db-prod-01.internal
username: ycrash_monitor
agentOnDbHost: true`)

		warnings, err := p.Validate()
		require.NoError(t, err)

		assert.True(t, p.AgentOnDBHost)
		require.Len(t, warnings, 3, "the database and frequency defaults warn too")
		assert.Contains(t, strings.Join(warnings, " "), "postgres.agentOnDbHost=true")
	})
}
