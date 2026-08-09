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

// Postgres is a PostgreSQL capture target and the window it is sampled over.
type Postgres struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
	SSLMode  string `yaml:"sslmode"`

	// CaptureDuration is a pointer because `captureDuration: 0s` and an omitted
	// key both decode to zero on a value field: nil is absent and takes the
	// default, where a non-nil zero is a configuration error.
	CaptureDuration *Duration `yaml:"captureDuration"`
}

// Defaults applied by Validate when the corresponding key is omitted.
const (
	DefaultPostgresPort = 5432

	// DefaultPostgresDatabase exists on effectively every cluster.
	DefaultPostgresDatabase = "postgres"

	// DefaultPostgresSSLMode is stricter than libpq's own.
	DefaultPostgresSSLMode = "require"

	// DefaultPostgresCaptureDuration matches the host capture's own span.
	DefaultPostgresCaptureDuration = 120 * time.Second

	// MaxPostgresCaptureDuration bounds a window whatever the file asks for: it
	// is a load commitment against a shared database.
	MaxPostgresCaptureDuration = 600 * time.Second
)

var postgresSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

var postgresPlaintextCapableSSLModes = []string{"disable", "allow", "prefer"}

var postgresEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func (p *Postgres) IsConfigured() bool { return p != nil }

// String redacts the password. It carries captureDuration because the
// effective-configuration echo is the only place an operator reads it back.
func (p *Postgres) String() string {
	if p == nil {
		return "<nil>"
	}

	password := `""`
	if p.Password != "" {
		password = "<redacted>"
	}

	// Non-nil after Validate; this covers a block rendered before it.
	window := fmt.Sprintf("(unset, defaults to %s)", DefaultPostgresCaptureDuration)
	if p.CaptureDuration != nil {
		window = p.CaptureDuration.String()
	}

	return fmt.Sprintf(
		"host=%q port=%d database=%q username=%q password=%s sslmode=%s captureDuration=%s",
		p.Host, p.Port, p.Database, p.Username, password, p.SSLMode, window,
	)
}

// GoString redacts under %#v, which ignores String - and capture.WrapRun formats
// a failing task with that verb into an agent log that is itself uploaded.
//
// The receiver is a value because fmt can only reach GoString on a nested,
// non-addressable struct field through the value method set.
func (p Postgres) GoString() string {
	return "config.Postgres{" + p.String() + "}"
}

// Validate normalizes the block in place. Single-call by contract: it applies
// defaults destructively and emits one-shot warnings.
func (p *Postgres) Validate() (warnings []string, err error) {
	if p == nil {
		return nil, nil
	}

	// Password is deliberately not trimmed: a quoted YAML scalar preserves
	// surrounding whitespace, and a password must reach the driver byte-exact.
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
	// that can be honoured in part, where 0s expresses nothing.
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

// expandPostgresEnvRefs returns one error per ${NAME} that could not be resolved.
func expandPostgresEnvRefs(raw string) (string, []error) {
	if raw == "" {
		return raw, nil
	}

	var errs []error

	reported := map[string]bool{}

	expanded := postgresEnvRef.ReplaceAllStringFunc(raw, func(ref string) string {
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
// what to capture, and "present but empty" is the better error for it.
func (p *Postgres) isZero() bool {
	return p.Host == "" &&
		p.Port == 0 &&
		p.Database == "" &&
		p.Username == "" &&
		p.Password == "" &&
		p.SSLMode == ""
}
