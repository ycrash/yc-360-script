package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Postgres holds the connection details for a PostgreSQL capture target, and
// the window the sampled artifacts are captured over.
type Postgres struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`

	// CaptureDuration is how long the sampling window stays open.
	//
	// A pointer because an explicit `captureDuration: 0s` and an omitted key
	// both decode to zero on a value field, and Validate could not tell the
	// typo from the default. nil is absent and takes the default; a non-nil
	// zero is a configuration error.
	CaptureDuration *Duration `yaml:"captureDuration"`
}

// Defaults applied by Validate when the corresponding key is omitted.
const (
	// DefaultPostgresPort is Postgres's universal default port.
	DefaultPostgresPort = 5432

	// DefaultPostgresDatabase is the database that exists on
	// effectively every cluster.
	DefaultPostgresDatabase = "postgres"

	// DefaultPostgresSSLMode is `require` rather than libpq's own.
	DefaultPostgresSSLMode = "require"

	// DefaultPostgresCaptureDuration is the sampling window when
	// captureDuration is omitted, matching the host capture's own span.
	DefaultPostgresCaptureDuration = 120 * time.Second

	// MaxPostgresCaptureDuration is the ceiling an over-large window is clamped
	// to. A window is a load commitment against a shared database, so it is
	// bounded whatever the file asks for.
	MaxPostgresCaptureDuration = 600 * time.Second
)

// postgresSSLModes is libpq's set, listed in libpq's order.
var postgresSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

var postgresPlaintextCapableSSLModes = []string{"disable", "allow", "prefer"}

// postgresEnvRef matches a ${NAME} environment-variable reference.
var postgresEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// IsConfigured reports whether a `postgres:` block was supplied at all.
func (p *Postgres) IsConfigured() bool { return p != nil }

// String renders the target for logs with the password redacted. It carries
// captureDuration because the effective-configuration echo is the only place an
// operator reads back the one setting that turns a two-second run into a
// ten-minute one.
func (p *Postgres) String() string {
	if p == nil {
		return "<nil>"
	}

	password := `""`
	if p.Password != "" {
		password = "<redacted>"
	}

	// Non-nil after Validate. The fallback covers a block rendered before
	// validation, and says so rather than inventing a value.
	window := fmt.Sprintf("(unset, defaults to %s)", DefaultPostgresCaptureDuration)
	if p.CaptureDuration != nil {
		window = p.CaptureDuration.String()
	}

	return fmt.Sprintf(
		"host=%q port=%d database=%q username=%q password=%s sslmode=%s captureDuration=%s",
		p.Host, p.Port, p.Database, p.Username, password, p.SSLMode, window,
	)
}

// GoString renders the block for %#v, which ignores String and prints struct
// fields raw - and capture.WrapRun formats a failing task with that verb into an
// agent log that is itself uploaded.
//
// The receiver is a value because fmt can only reach GoString on a nested,
// non-addressable struct field through the value method set. %v and %+v are
// String's, whose pointer receiver is why every holder of this block holds a
// pointer.
func (p Postgres) GoString() string {
	return "config.Postgres{" + p.String() + "}"
}

// Validate normalizes the block in place and validates the result.
//
// It is single-call by contract: it applies defaults destructively and
// emits one-shot warnings, and running it twice is not supported.
func (p *Postgres) Validate() (warnings []string, err error) {
	if p == nil {
		return nil, nil
	}

	// Password is deliberately absent from this list. A quoted YAML scalar
	// preserves leading and trailing whitespace, and a password must reach the
	// driver byte-exact.
	p.Host = strings.TrimSpace(p.Host)
	p.Database = strings.TrimSpace(p.Database)
	p.Username = strings.TrimSpace(p.Username)
	p.SSLMode = strings.ToLower(strings.TrimSpace(p.SSLMode))

	var errs []error

	var expandErrs []error
	p.Password, expandErrs = expandPostgresEnvRefs(p.Password)
	errs = append(errs, expandErrs...)

	if p.isZero() {
		return nil, errors.New("postgres block is present but empty or has no recognised keys " +
			"(valid keys: host, port, database, username, password, sslmode, captureDuration)")
	}

	if p.Port == 0 {
		p.Port = DefaultPostgresPort
	}
	if p.Database == "" {
		p.Database = DefaultPostgresDatabase

		warnings = append(warnings, fmt.Sprintf(
			"postgres.database not set - defaulting to %q. Table health covers only the connected "+
				"database, and query statistics require connecting to a database where "+
				"pg_stat_statements is installed - name the application database to capture both.",
			DefaultPostgresDatabase,
		))
	}
	if p.SSLMode == "" {
		p.SSLMode = DefaultPostgresSSLMode
	}

	// Clamped above the ceiling, rejected at or below zero: 900s is an intent
	// that can be honoured in part, where 0s expresses nothing and silently
	// defaulting it would hide a typo.
	switch {
	case p.CaptureDuration == nil:
		p.CaptureDuration = newDuration(DefaultPostgresCaptureDuration)

	case p.CaptureDuration.Duration() <= 0:
		errs = append(errs, fmt.Errorf(
			"postgres.captureDuration is %s - it must be positive (omit the key for the %s default)",
			p.CaptureDuration, DefaultPostgresCaptureDuration))

	case p.CaptureDuration.Duration() > MaxPostgresCaptureDuration:
		warnings = append(warnings, fmt.Sprintf(
			"postgres.captureDuration %s exceeds the %s maximum - capturing for %s instead. "+
				"The window is a load commitment against a shared database.",
			p.CaptureDuration, MaxPostgresCaptureDuration, MaxPostgresCaptureDuration))

		p.CaptureDuration = newDuration(MaxPostgresCaptureDuration)
	}

	if p.Host == "" {
		errs = append(errs, errors.New("postgres.host is required"))
	}
	if p.Username == "" {
		errs = append(errs, errors.New("postgres.username is required"))
	}

	if p.Port < 1 || p.Port > 65535 {
		errs = append(errs, fmt.Errorf("postgres.port %d is out of range (1-65535)", p.Port))
	}

	switch {
	case !slices.Contains(postgresSSLModes, p.SSLMode):
		errs = append(errs, fmt.Errorf("postgres.sslmode %q is invalid (valid values: %s)",
			p.SSLMode, strings.Join(postgresSSLModes, ", ")))

	case p.SSLMode == "disable":
		warnings = append(warnings, "postgres.sslmode=disable - the connection will not be "+
			"encrypted; credentials and captured query text would cross the network in plaintext.")

	case slices.Contains(postgresPlaintextCapableSSLModes, p.SSLMode):
		warnings = append(warnings, fmt.Sprintf("postgres.sslmode=%s - the connection may not be "+
			"encrypted; credentials and captured query text could cross the network in plaintext.",
			p.SSLMode))
	}

	return warnings, errors.Join(errs...)
}

// expandPostgresEnvRefs substitutes every ${NAME} reference in raw with the
// value of that environment variable, returning the expanded string and one
// error per reference that could not be resolved.
func expandPostgresEnvRefs(raw string) (string, []error) {
	if raw == "" {
		return raw, nil
	}

	var errs []error

	reported := map[string]bool{}

	expanded := postgresEnvRef.ReplaceAllStringFunc(raw, func(ref string) string {
		// the name between "${" ... "}"
		name := ref[2 : len(ref)-1]

		value, ok := os.LookupEnv(name)
		switch {
		case !ok:
			if !reported[name] {
				reported[name] = true
				errs = append(errs, fmt.Errorf(
					"postgres.password references ${%s}, which is not set in the environment", name))
			}
			return ref
		case value == "":
			if !reported[name] {
				reported[name] = true
				errs = append(errs, fmt.Errorf(
					"postgres.password references ${%s}, which is set but empty", name))
			}
			return ref
		default:
			return value
		}
	})

	return expanded, errs
}

func newDuration(d time.Duration) *Duration {
	wrapped := Duration(d)
	return &wrapped
}

// isZero reports whether the block names no target. CaptureDuration is
// deliberately absent: a block whose only key is captureDuration has not said
// what to capture, and "present but empty" is the error worth showing rather
// than host- and username-required.
func (p *Postgres) isZero() bool {
	return p.Host == "" &&
		p.Port == 0 &&
		p.Database == "" &&
		p.Username == "" &&
		p.Password == "" &&
		p.SSLMode == ""
}
