package config

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// MySQL is a MySQL capture target and the window it is sampled over. In the file,
// mysql: is a list of targets with one entry, the same shape as postgres:.
//
// database is optional: MySQL has no database that always exists to default to,
// and none is needed to connect - every capture reads performance_schema, which
// is instance-wide. Named, it is the connection's default schema, and a name the
// server does not know refuses the connection.
type MySQL struct {
	DBTarget `yaml:",inline"`
}

// mysqlKey is the block's key, which starts every message about it.
const mysqlKey = "mysql"

const DefaultMySQLPort = 3306

// mysqlExplainModes is all alone: MySQL has no auto_explain, so there is no logged
// tier, and every plan is one the agent asks the server for.
var mysqlExplainModes = []string{ExplainAll}

// UnmarshalYAML reads mysql: as a list of targets. Any other shape still decodes,
// so that Validate can say what to write instead.
func (m *MySQL) UnmarshalYAML(value *yaml.Node) error {
	type target MySQL // no methods, so decoding an entry does not come back here

	return m.decodeTargetList(mysqlKey, value, (*target)(m))
}

func (m *MySQL) IsConfigured() bool { return m != nil }

// String redacts the password.
func (m *MySQL) String() string {
	if m == nil {
		return "<nil>"
	}

	return m.targetString()
}

// GoString redacts under %#v (String is skipped); capture.WrapRun logs failing
// tasks via %#v into an uploaded log. Value receiver: reaches non-addressable struct fields.
func (m MySQL) GoString() string {
	return "config.MySQL{" + m.String() + "}"
}

// ExplainMode is the run's plan-capture intent as a token: all, or ExplainOff for
// an omitted key. my_metadata.txt records it.
func (m *MySQL) ExplainMode() string {
	if m == nil {
		return ExplainOff
	}

	return m.DBTarget.ExplainMode()
}

// checkCAFile makes a CA file's path absolute and reads it now. The capture runs
// from the bundle's own directory, where a relative path would no longer resolve,
// and a file the driver cannot use would otherwise fail every connection only once
// the window opens.
func checkCAFile(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, err
	}

	pem, err := os.ReadFile(abs)
	if err != nil {
		return abs, err
	}

	if !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return abs, fmt.Errorf("%s holds no PEM certificate", abs)
	}

	return abs, nil
}

// Validate normalizes the block in place; not idempotent (destructive defaults, one-shot warnings).
func (m *MySQL) Validate() (warnings []string, err error) {
	if m == nil {
		return nil, nil
	}

	if err := m.decodeErr(); err != nil {
		return nil, err
	}

	m.trim()

	var errs []error

	errs = append(errs, m.expandPassword(mysqlKey)...)

	if m.isZero() {
		return nil, emptyBlockError(mysqlKey)
	}

	if m.Port == 0 {
		m.Port = DefaultMySQLPort
	}

	windowWarnings, windowErrs := m.validateWindow(mysqlKey, "my_sessions.txt, my_lock_waits.txt")
	warnings = append(warnings, windowWarnings...)
	errs = append(errs, windowErrs...)

	errs = append(errs, m.validateIdentity(mysqlKey)...)

	tlsWarnings, tlsErrs := m.validateTLS(mysqlKey)
	warnings = append(warnings, tlsWarnings...)
	errs = append(errs, tlsErrs...)

	if m.TLSVerified() && m.TLS.CAFile != "" {
		caFile, err := checkCAFile(m.TLS.CAFile)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s.tls.caFile: %w", mysqlKey, err))
		}

		m.TLS.CAFile = caFile
	}

	switch {
	case m.Explain == "":
		// Omission is the off switch, so there is nothing to check and nothing to say.

	case slices.Contains(explainBooleans, m.Explain):
		errs = append(errs, fmt.Errorf(
			"mysql.explain is %q - it takes %q; omit the key to capture no plans",
			m.Explain, ExplainAll))

	case m.Explain == ExplainLogged:
		errs = append(errs, fmt.Errorf(
			"mysql.explain is %q - MySQL has no auto_explain, so the server logs no plans to copy; "+
				"use %q, or omit the key to capture no plans", ExplainLogged, ExplainAll))

	case !slices.Contains(mysqlExplainModes, m.Explain):
		errs = append(errs, fmt.Errorf("mysql.explain %q is invalid (valid values: %s)",
			m.Explain, ExplainAll))

	case m.Explain == ExplainAll:
		warnings = append(warnings, "mysql.explain=all - each statement's query_sample_text, with "+
			"the application's real values in it, will be submitted back to the database as "+
			"EXPLAIN FORMAT=JSON statements; the plans written to the bundle have their literal values replaced.")
	}

	if m.AgentOnDBHost {
		warnings = append(warnings, agentOnDBHostWarning(mysqlKey))
	}

	return warnings, errors.Join(errs...)
}
