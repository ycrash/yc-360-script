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

	// TLS is how the connection is encrypted and checked. nil (key omitted) is
	// encrypted with the certificate not verified; Validate fills it in.
	TLS *PostgresTLS `yaml:"tls"`

	// SSLMode is decoded only to be refused: the tls: block replaced it, and an
	// unknown key would otherwise be dropped without a word, turning
	// sslmode: verify-full into an unverified connection.
	SSLMode string `yaml:"sslmode"`

	// CaptureDuration is how long to capture. Required: nil (key omitted) and 0s are
	// configuration errors.
	CaptureDuration *Duration `yaml:"captureDuration"`

	// Frequency is how often the periodic artifacts are sampled, and the source of
	// the three speeds (PostgresFastFrequency, PostgresExpensiveFrequency). Pointer:
	// nil (key omitted) takes the default; 0s is a configuration error. Whatever the
	// value, the opening and closing samples are always taken, so a frequency no
	// shorter than the window leaves exactly those two - and Validate says so.
	Frequency *Duration `yaml:"frequency"`

	// Explain selects which plan-capture tiers run. Empty (key omitted) captures no
	// plans: EXPLAIN carries a privilege and a load cost the other nine artifacts do not.
	Explain string `yaml:"explain"`

	// AgentOnDBHost declares that this machine runs the database, for the one case
	// the probe cannot reach: the database is down, so there is no backend to look
	// for - exactly when host readings matter most. A measurement always wins.
	AgentOnDBHost bool `yaml:"agentOnDbHost"`
}

// PostgresTLS is the tls: block.
type PostgresTLS struct {
	// Enabled: nil (key omitted) is true. false connects in plaintext.
	Enabled *bool `yaml:"enabled"`

	// VerifyServerCertificate checks the certificate's chain and that it names
	// the server. nil (key omitted) is false.
	VerifyServerCertificate *bool `yaml:"verifyServerCertificate"`

	// CAFile is what the certificate must chain to; empty is the system's trust
	// store. Used only when verifying.
	CAFile string `yaml:"caFile"`

	// ServerName is the name the certificate must carry, and the one sent to the
	// server; empty is host. For connecting by IP to a certificate issued for a name.
	ServerName string `yaml:"serverName"`
}

// TLSEnabled and TLSVerified read the block with its defaults, whether or not
// Validate has filled them in.
func (p *Postgres) TLSEnabled() bool {
	return p.TLS == nil || p.TLS.Enabled == nil || *p.TLS.Enabled
}

func (p *Postgres) TLSVerified() bool {
	return p.TLSEnabled() && p.TLS != nil && p.TLS.VerifyServerCertificate != nil && *p.TLS.VerifyServerCertificate
}

const (
	DefaultPostgresPort = 5432

	// DefaultPostgresDatabase exists on effectively every cluster.
	DefaultPostgresDatabase = "postgres"

	// MaxPostgresCaptureDuration caps captureDuration: a load commitment against a
	// shared database. Two hours is the longest window supported; the host
	// files stretch with it, so a long window's netstat and ps readings are far apart.
	MaxPostgresCaptureDuration = 2 * time.Hour

	// DefaultPostgresFrequency is 5m. A short incident capture that wants samples
	// between the endpoints sets frequency itself (30s, for example), and one that
	// does not is warned.
	DefaultPostgresFrequency = 5 * time.Minute

	// MaxPostgresFastFrequency caps the fast speed, session and lock state: a
	// blocking episode shorter than one sampling interval can pass unseen.
	MaxPostgresFastFrequency = 15 * time.Second

	// MinPostgresExpensiveFrequency floors the expensive speed: whole-table reads
	// and settings, costly to take and slow to change.
	MinPostgresExpensiveFrequency = 5 * time.Minute

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
	window := "(unset, required)"
	if p.CaptureDuration != nil {
		window = p.CaptureDuration.String()
	}

	frequency := fmt.Sprintf("(unset, defaults to %s)", DefaultPostgresFrequency)
	if p.Frequency != nil {
		frequency = p.Frequency.String()
	}

	caFile, serverName := "", ""
	if p.TLS != nil {
		caFile, serverName = p.TLS.CAFile, p.TLS.ServerName
	}

	return fmt.Sprintf(
		"host=%q port=%d database=%q username=%q password=%s "+
			"tls.enabled=%t tls.verifyServerCertificate=%t tls.caFile=%q tls.serverName=%q "+
			"captureDuration=%s frequency=%s explain=%s agentOnDbHost=%t",
		p.Host, p.Port, p.Database, p.Username, password,
		p.TLSEnabled(), p.TLSVerified(), caFile, serverName,
		window, frequency, p.ExplainMode(), p.AgentOnDBHost,
	)
}

// PostgresFastFrequency is the fast speed for a frequency: at most 15s.
func PostgresFastFrequency(frequency time.Duration) time.Duration {
	return min(frequency, MaxPostgresFastFrequency)
}

// PostgresExpensiveFrequency is the expensive speed for a frequency: at least 5m.
func PostgresExpensiveFrequency(frequency time.Duration) time.Duration {
	return max(frequency, MinPostgresExpensiveFrequency)
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

	if p.TLS != nil {
		p.TLS.CAFile = strings.TrimSpace(p.TLS.CAFile)
		p.TLS.ServerName = strings.TrimSpace(p.TLS.ServerName)
	}
	p.Explain = strings.ToLower(strings.TrimSpace(p.Explain))

	var errs []error

	var expandErrs []error
	p.Password, expandErrs = expandPostgresEnvRefs(p.Password)
	errs = append(errs, expandErrs...)

	if p.isZero() {
		return nil, errors.New("postgres block is present but empty or has no recognised keys " +
			"(valid keys: host, port, database, username, password, tls, captureDuration, " +
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
	// Over-ceiling clamps and warns (partial intent); missing or non-positive is rejected outright.
	switch {
	case p.CaptureDuration == nil:
		errs = append(errs, fmt.Errorf(
			"postgres.captureDuration is required - how long to capture, for example 2m (at most %s)",
			MaxPostgresCaptureDuration))

	case p.CaptureDuration.Duration() <= 0:
		errs = append(errs, fmt.Errorf(
			"postgres.captureDuration is %s - it must be positive (at most %s)",
			p.CaptureDuration, MaxPostgresCaptureDuration))

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

	// The bookend is always taken, so this is a warning and not an error - but a
	// short window with the default frequency lands here, and a capture that was
	// never told a cadence would otherwise be silently two samples of most files.
	if p.CaptureDuration != nil && p.Frequency.Duration() > 0 && p.CaptureDuration.Duration() > 0 &&
		p.Frequency.Duration() >= p.CaptureDuration.Duration() {
		warnings = append(warnings, p.bookendWarning(frequencyDefaulted))
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

	if p.SSLMode != "" {
		errs = append(errs, fmt.Errorf("postgres.sslmode is no longer accepted - use the tls: block: %s",
			sslModeReplacement(p.SSLMode)))
	}

	tlsWarnings, tlsErrs := p.validateTLS()
	warnings = append(warnings, tlsWarnings...)
	errs = append(errs, tlsErrs...)

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
		warnings = append(warnings, "postgres.explain=all - captured query text, with the "+
			"parameter values the server logged, will be submitted back to the database as "+
			"EXPLAIN statements; the plans written to the bundle have their literal values replaced.")
	}

	if p.AgentOnDBHost {
		warnings = append(warnings, "postgres.agentOnDbHost=true - this machine's process list, "+
			"connection table, kernel messages and kernel settings will be captured and filed "+
			"under the database whenever the run cannot establish for itself that the two are "+
			"the same machine. A run that establishes they are not still skips them.")
	}

	return warnings, errors.Join(errs...)
}

// bookendWarning says which files keep only the opening and closing samples when
// frequency meets or exceeds the window. The fast speed is capped below
// frequency, so session state still samples within a window longer than it.
func (p *Postgres) bookendWarning(frequencyDefaulted bool) string {
	window := p.CaptureDuration.Duration()
	fast := PostgresFastFrequency(p.Frequency.Duration())

	consequence := "normal-speed and expensive-speed files will produce only the opening and " +
		"closing samples, no samples in between"
	if fast < window {
		consequence += fmt.Sprintf("; fast-speed files (pg_sessions.txt) are unaffected, "+
			"sampled every %s", fast)
	}

	if frequencyDefaulted {
		return fmt.Sprintf("postgres.frequency is unset and defaults to %s, which meets or exceeds "+
			"postgres.captureDuration (%s) - %s. Set postgres.frequency (for example 30s) to sample "+
			"the normal-speed files within the window.",
			DefaultPostgresFrequency, p.CaptureDuration, consequence)
	}

	return fmt.Sprintf("postgres.frequency (%s) meets or exceeds postgres.captureDuration (%s) - %s.",
		p.Frequency, p.CaptureDuration, consequence)
}

// validateTLS fills in the block's defaults, so what the run uses is what it
// says, and refuses the combinations that could only be a mistake.
func (p *Postgres) validateTLS() (warnings []string, errs []error) {
	if p.TLS == nil {
		p.TLS = &PostgresTLS{}
	}

	enabled, verified := p.TLSEnabled(), p.TLSVerified()
	verifyAsked := p.TLS.VerifyServerCertificate != nil && *p.TLS.VerifyServerCertificate

	switch {
	case !enabled && verifyAsked:
		errs = append(errs, errors.New("postgres.tls.verifyServerCertificate is true but "+
			"tls.enabled is false - a certificate can only be checked over TLS"))

	case !enabled && (p.TLS.CAFile != "" || p.TLS.ServerName != ""):
		errs = append(errs, errors.New("postgres.tls.caFile and tls.serverName have no effect "+
			"when tls.enabled is false - remove them, or enable TLS"))

	case !enabled:
		warnings = append(warnings, "postgres.tls.enabled=false - the connection will not be "+
			"encrypted; credentials and captured query text would cross the network in plaintext.")

	case !verified && p.TLS.CAFile != "":
		errs = append(errs, errors.New("postgres.tls.caFile is set but "+
			"tls.verifyServerCertificate is not - the CA is used only to verify the server's "+
			"certificate; set verifyServerCertificate: true, or remove caFile"))
	}

	p.TLS.Enabled = &enabled
	p.TLS.VerifyServerCertificate = &verified

	return warnings, errs
}

// sslModeReplacement is the tls: form of an sslmode value. Three have none:
// prefer and allow fall back to plaintext, and verify-ca checks the chain
// without the name, which serverName makes unnecessary.
func sslModeReplacement(mode string) string {
	switch mode {
	case "disable":
		return "sslmode: disable is tls: {enabled: false}"

	case "require":
		return "sslmode: require is tls: {enabled: true}, which is also what omitting tls: gives"

	case "verify-full":
		return "sslmode: verify-full is tls: {enabled: true, verifyServerCertificate: true}, " +
			"with caFile: set to the file sslrootcert named, if any"

	case "verify-ca":
		return "sslmode: verify-ca has no tls: form - use tls: {enabled: true, " +
			"verifyServerCertificate: true}, which also checks the certificate names the server; " +
			"set serverName to that name when connecting by IP"

	case "prefer", "allow":
		return fmt.Sprintf("sslmode: %s has no tls: form, since it can fall back to plaintext - "+
			"tls: {enabled: true} requires encryption, tls: {enabled: false} is plaintext", mode)
	}

	return fmt.Sprintf("sslmode: %q is not a mode; the tls: block takes enabled, "+
		"verifyServerCertificate, caFile and serverName", mode)
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
		p.TLS == nil &&
		p.SSLMode == ""
}
