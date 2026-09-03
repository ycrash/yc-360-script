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

	// Pointer: nil (key omitted) takes the default; 0s is a configuration error.
	CaptureDuration *Duration `yaml:"captureDuration"`

	// Frequency is how often the periodic artifacts are sampled. Pointer: nil (key
	// omitted) takes the default; 0s is a configuration error. Whatever the value,
	// the opening and closing samples are always taken, so a frequency no shorter
	// than the window leaves exactly those two - and Validate says so.
	Frequency *Duration `yaml:"frequency"`

	// Explain selects which plan-capture tiers run. Empty (key omitted) captures no
	// plans: EXPLAIN carries a privilege and a load cost the other nine artifacts do not.
	Explain string `yaml:"explain"`

	// AgentOnDBHost declares that this machine runs the database, for the one case
	// the probe cannot reach: the database is down, so there is no backend to look
	// for - exactly when host readings matter most. A measurement always wins.
	AgentOnDBHost bool `yaml:"agentOnDbHost"`
}

const (
	DefaultPostgresPort = 5432

	// DefaultPostgresDatabase exists on effectively every cluster.
	DefaultPostgresDatabase = "postgres"

	// DefaultPostgresSSLMode is stricter than libpq's own.
	DefaultPostgresSSLMode = "require"

	// DefaultPostgresCaptureDuration matches SCRIPT_SPAN, the application capture's
	// nominal span - not the host collectors' real one: top and vmstat run ~20s.
	DefaultPostgresCaptureDuration = 120 * time.Second

	// MaxPostgresCaptureDuration caps captureDuration: a load commitment against a
	// shared database. Two hours is the longest window supported; the host
	// files stretch with it, so a long window's netstat and ps readings are far apart.
	MaxPostgresCaptureDuration = 2 * time.Hour

	// DefaultPostgresFrequency is 5m. Longer than the default window, so an
	// incident capture that wants samples between the endpoints sets frequency
	// itself (30s, for example), and one that does not is warned.
	DefaultPostgresFrequency = 5 * time.Minute

	// MinPostgresFrequency floors frequency. It equals the capture's per-statement
	// timeout, pinned by a test there, so a maxed-out sample can never outrun the
	// tick behind it; below it the timeline could not catch up under load.
	MinPostgresFrequency = 10 * time.Second

	// ExplainLogged captures only the plans the server itself logged - nothing is
	// submitted back to the database.
	ExplainLogged = "logged"

	// ExplainAll adds the two estimated tiers, which submit EXPLAIN statements.
	ExplainAll = "all"

	// ExplainOff is what an omitted key reports as. It is not an accepted input:
	// presence is the switch, so turning the feature off means deleting the line.
	ExplainOff = "off"
)

var postgresSSLModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

var postgresPlaintextCapableSSLModes = []string{"disable", "allow", "prefer"}

var postgresExplainModes = []string{ExplainLogged, ExplainAll}

// postgresExplainBooleans are what a human types instead of omitting the key; yaml.v3
// passes them through, where the generic error would not say to delete the line.
var postgresExplainBooleans = []string{"true", "false", "on", "off", "yes", "no"}

var postgresEnvRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

func (p *Postgres) IsConfigured() bool { return p != nil }

// String redacts the password.
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

	frequency := fmt.Sprintf("(unset, defaults to %s)", DefaultPostgresFrequency)
	if p.Frequency != nil {
		frequency = p.Frequency.String()
	}

	return fmt.Sprintf(
		"host=%q port=%d database=%q username=%q password=%s sslmode=%s captureDuration=%s frequency=%s explain=%s agentOnDbHost=%t",
		p.Host, p.Port, p.Database, p.Username, password, p.SSLMode, window, frequency, p.ExplainMode(), p.AgentOnDBHost,
	)
}

// ExplainMode is the run's plan-capture intent as a token: an accepted value, or
// ExplainOff for an omitted key. pg_metadata.txt records it.
func (p *Postgres) ExplainMode() string {
	if p == nil || p.Explain == "" {
		return ExplainOff
	}

	return p.Explain
}

// GoString redacts under %#v (String is skipped); capture.WrapRun logs failing
// tasks via %#v into an uploaded log. Value receiver: reaches non-addressable struct fields.
func (p Postgres) GoString() string {
	return "config.Postgres{" + p.String() + "}"
}

// Validate normalizes the block in place; not idempotent (destructive defaults, one-shot warnings).
func (p *Postgres) Validate() (warnings []string, err error) {
	if p == nil {
		return nil, nil
	}

	// Password is not trimmed: it must reach the driver byte-exact.
	p.Host = strings.TrimSpace(p.Host)
	p.Database = strings.TrimSpace(p.Database)
	p.Username = strings.TrimSpace(p.Username)
	p.SSLMode = strings.ToLower(strings.TrimSpace(p.SSLMode))
	p.Explain = strings.ToLower(strings.TrimSpace(p.Explain))

	var errs []error

	var expandErrs []error
	p.Password, expandErrs = expandPostgresEnvRefs(p.Password)
	errs = append(errs, expandErrs...)

	if p.isZero() {
		return nil, errors.New("postgres block is present but empty or has no recognised keys " +
			"(valid keys: host, port, database, username, password, sslmode, captureDuration, " +
			"frequency, explain, agentOnDbHost)")
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

	// Over-ceiling clamps and warns (partial intent); non-positive is rejected outright.
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

	// Under-floor clamps and warns, as the ceiling does above; non-positive is rejected.
	frequencyDefaulted := false

	switch {
	case p.Frequency == nil:
		p.Frequency = newDuration(DefaultPostgresFrequency)
		frequencyDefaulted = true

	case p.Frequency.Duration() <= 0:
		errs = append(errs, fmt.Errorf(
			"postgres.frequency is %s - it must be positive (omit the key for the %s default)",
			p.Frequency, DefaultPostgresFrequency))

	case p.Frequency.Duration() < MinPostgresFrequency:
		warnings = append(warnings, fmt.Sprintf(
			"postgres.frequency %s is below the %s minimum - sampling every %s instead. "+
				"A sample's statements are bounded at %s, so a faster cadence would let one slow "+
				"sample outrun the tick behind it.",
			p.Frequency, MinPostgresFrequency, MinPostgresFrequency, MinPostgresFrequency))

		p.Frequency = newDuration(MinPostgresFrequency)
	}

	// The bookend is always taken, so this is a warning and not an error - but on
	// the default window the default frequency lands here, and a capture that was
	// never told a cadence would otherwise be silently two samples of everything.
	if p.Frequency.Duration() > 0 && p.CaptureDuration.Duration() > 0 &&
		p.Frequency.Duration() >= p.CaptureDuration.Duration() {
		if frequencyDefaulted {
			warnings = append(warnings, fmt.Sprintf(
				"postgres.frequency is unset and defaults to %s, which is not shorter than the %s "+
					"window - only the opening and closing samples will be taken. Set "+
					"postgres.frequency (for example 30s) to sample within the window.",
				DefaultPostgresFrequency, p.CaptureDuration))
		} else {
			warnings = append(warnings, fmt.Sprintf(
				"postgres.frequency %s is not shorter than the %s window - only the opening and "+
					"closing samples will be taken.",
				p.Frequency, p.CaptureDuration))
		}
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

	switch {
	case p.Explain == "":
		// Omission is the off switch, so there is nothing to check and nothing to say.

	case slices.Contains(postgresExplainBooleans, p.Explain):
		errs = append(errs, fmt.Errorf(
			"postgres.explain is %q - it takes %q or %q; omit the key to capture no plans",
			p.Explain, ExplainLogged, ExplainAll))

	case !slices.Contains(postgresExplainModes, p.Explain):
		errs = append(errs, fmt.Errorf("postgres.explain %q is invalid (valid values: %s)",
			p.Explain, strings.Join(postgresExplainModes, ", ")))

	case p.Explain == ExplainAll:
		warnings = append(warnings, "postgres.explain=all - captured query text will be submitted "+
			"back to the database as EXPLAIN statements, and captured plans contain literal "+
			"parameter values from your data.")
	}

	if p.AgentOnDBHost {
		warnings = append(warnings, "postgres.agentOnDbHost=true - this machine's process list, "+
			"connection table, kernel messages and kernel settings will be captured and filed "+
			"under the database whenever the run cannot establish for itself that the two are "+
			"the same machine. A run that establishes they are not still skips them.")
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

// isZero reports whether the block names no target. CaptureDuration and Frequency
// are excluded: a window or a cadence alone hasn't said what to capture, so that
// case gets the "empty" error.
func (p *Postgres) isZero() bool {
	return p.Host == "" &&
		p.Port == 0 &&
		p.Database == "" &&
		p.Username == "" &&
		p.Password == "" &&
		p.SSLMode == ""
}
