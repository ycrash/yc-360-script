package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Postgres is a PostgreSQL capture target and the window it is sampled over. In the
// file, postgres: is a list of targets with one entry.
type Postgres struct {
	DBTarget `yaml:",inline"`

	// SSLMode is decoded only to be refused: the tls: block replaced it, and an
	// unknown key would otherwise be dropped without a word, turning
	// sslmode: verify-full into an unverified connection.
	SSLMode string `yaml:"sslmode"`
}

// postgresKey is the block's key, which starts every message about it.
const postgresKey = "postgres"

// UnmarshalYAML reads postgres: as a list of targets. Any other shape still decodes,
// so that Validate can say what to write instead.
func (p *Postgres) UnmarshalYAML(value *yaml.Node) error {
	type target Postgres // no methods, so decoding an entry does not come back here

	return p.decodeTargetList(postgresKey, value, func(entry *yaml.Node) error {
		return entry.Decode((*target)(p))
	})
}

// PostgresTLS is the tls: block, the same for every engine.
type PostgresTLS = TargetTLS

const (
	DefaultPostgresPort = 5432

	// DefaultPostgresDatabase exists on effectively every cluster.
	DefaultPostgresDatabase = "postgres"

	// The window's limits, the same for every engine; see DBTarget.
	MaxPostgresCaptureDuration    = MaxCaptureDuration
	DefaultPostgresFrequency      = DefaultFrequency
	MaxPostgresFastFrequency      = MaxFastFrequency
	MinPostgresExpensiveFrequency = MinExpensiveFrequency
	MinPostgresFrequency          = MinFrequency
)

var postgresExplainModes = []string{ExplainLogged, ExplainAll}

func (p *Postgres) IsConfigured() bool { return p != nil }

// String redacts the password.
func (p *Postgres) String() string {
	if p == nil {
		return "<nil>"
	}

	return p.targetString()
}

// PostgresFastFrequency is the fast speed for a frequency: at most 15s.
func PostgresFastFrequency(frequency time.Duration) time.Duration { return FastFrequency(frequency) }

// PostgresExpensiveFrequency is the expensive speed for a frequency: at least 5m.
func PostgresExpensiveFrequency(frequency time.Duration) time.Duration {
	return ExpensiveFrequency(frequency)
}

// ExplainMode is the run's plan-capture intent as a token: an accepted value, or
// ExplainOff for an omitted key. pg_metadata.txt records it.
func (p *Postgres) ExplainMode() string {
	if p == nil {
		return ExplainOff
	}

	return p.DBTarget.ExplainMode()
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

	if p.shapeErr != "" {
		return nil, errors.New(p.shapeErr)
	}

	p.trim()
	p.SSLMode = strings.ToLower(strings.TrimSpace(p.SSLMode))

	var errs []error

	errs = append(errs, p.expandPassword(postgresKey)...)

	if p.isZero() {
		return nil, emptyBlockError(postgresKey)
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

	windowWarnings, windowErrs := p.validateWindow(postgresKey, "pg_sessions.txt")
	warnings = append(warnings, windowWarnings...)
	errs = append(errs, windowErrs...)

	errs = append(errs, p.validateIdentity(postgresKey)...)

	if p.SSLMode != "" {
		errs = append(errs, fmt.Errorf("postgres.sslmode is no longer accepted - use the tls: block: %s",
			sslModeReplacement(p.SSLMode)))
	}

	tlsWarnings, tlsErrs := p.validateTLS(postgresKey)
	warnings = append(warnings, tlsWarnings...)
	errs = append(errs, tlsErrs...)

	switch {
	case p.Explain == "":
		// Omission is the off switch, so there is nothing to check and nothing to say.

	case slices.Contains(explainBooleans, p.Explain):
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
		warnings = append(warnings, agentOnDBHostWarning(postgresKey))
	}

	return warnings, errors.Join(errs...)
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

// isZero reports whether the block names no target; see DBTarget.isZero.
func (p *Postgres) isZero() bool {
	return p.DBTarget.isZero() && p.SSLMode == ""
}
