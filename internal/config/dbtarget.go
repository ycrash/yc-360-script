package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DBTarget is what every database block shares: target, credentials, TLS and the
// window. Engines embed it inline.
type DBTarget struct {
	// ID names the target; every sample block carries it as target_id=. Required.
	ID string `yaml:"id"`

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// TLS: nil is encrypted, unverified; Validate fills it in.
	TLS *TargetTLS `yaml:"tls"`

	// CaptureDuration is how long to capture. Required.
	CaptureDuration *Duration `yaml:"captureDuration"`

	// Frequency is the periodic cadence and the source of the three speeds. nil
	// takes the default; 0s is an error.
	Frequency *Duration `yaml:"frequency"`

	// Explain selects the plan-capture tiers; empty captures no plans.
	Explain string `yaml:"explain"`

	// AgentOnDBHost declares this is the database's machine, for when the probe
	// cannot run (database down). A measurement always wins.
	AgentOnDBHost bool `yaml:"agentOnDbHost"`

	// shapeErr is why the block was not a list of one target; Validate refuses it.
	shapeErr string

	// keyErrs are unknown keys; Validate refuses them.
	keyErrs []error
}

// TargetTLS is the tls: block.
type TargetTLS struct {
	// Enabled: nil (key omitted) is true. false connects in plaintext.
	Enabled *bool `yaml:"enabled"`

	// VerifyServerCertificate checks chain and name. nil is false.
	VerifyServerCertificate *bool `yaml:"verifyServerCertificate"`

	// CAFile is the trust root when verifying; empty is the system store.
	CAFile string `yaml:"caFile"`

	// ServerName is the certificate's expected name (and SNI); empty is host.
	ServerName string `yaml:"serverName"`
}

const (
	// MaxCaptureDuration caps captureDuration.
	MaxCaptureDuration = 2 * time.Hour

	// DefaultFrequency is 5m.
	DefaultFrequency = 5 * time.Minute

	// MaxFastFrequency caps the fast speed (session and lock state).
	MaxFastFrequency = 15 * time.Second

	// MinExpensiveFrequency floors the expensive speed (whole-table reads, settings).
	MinExpensiveFrequency = 5 * time.Minute

	// MinFrequency floors frequency at each engine's statement timeout (pinned by a
	// test in internal/capture), so a maxed-out sample cannot outrun the next tick.
	MinFrequency = 5 * time.Second

	// ExplainLogged captures only plans the server logged; nothing is submitted.
	ExplainLogged = "logged"

	// ExplainAll adds the estimated tiers, which submit EXPLAIN statements.
	ExplainAll = "all"

	// ExplainOff is what an omitted key reports; it is not an accepted input.
	ExplainOff = "off"
)

// FastFrequency is the fast speed for a frequency: at most 15s.
func FastFrequency(frequency time.Duration) time.Duration {
	return min(frequency, MaxFastFrequency)
}

// ExpensiveFrequency is the expensive speed for a frequency: at least 5m.
func ExpensiveFrequency(frequency time.Duration) time.Duration {
	return max(frequency, MinExpensiveFrequency)
}

// explainBooleans get an error saying to delete the line instead.
var explainBooleans = []string{"true", "false", "on", "off", "yes", "no"}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// TLSEnabled and TLSVerified read the block with its defaults.
func (t *DBTarget) TLSEnabled() bool {
	return t.TLS == nil || t.TLS.Enabled == nil || *t.TLS.Enabled
}

func (t *DBTarget) TLSVerified() bool {
	return t.TLSEnabled() && t.TLS != nil && t.TLS.VerifyServerCertificate != nil && *t.TLS.VerifyServerCertificate
}

// ExplainMode is the plan-capture intent, ExplainOff for an omitted key.
func (t *DBTarget) ExplainMode() string {
	if t == nil || t.Explain == "" {
		return ExplainOff
	}

	return t.Explain
}

// decodeTargetList decodes a list of one target into entry. Any other shape, or
// unknown keys, still decodes, with the reason left for Validate.
func (t *DBTarget) decodeTargetList(key string, value *yaml.Node, entry any) error {
	switch value.Kind {
	case yaml.MappingNode:
		t.shapeErr = key + ": is a list of targets - write this block as its one entry, " +
			"starting with - id: <a name for this target>"

		return nil

	case yaml.SequenceNode:
		switch len(value.Content) {
		case 0:
			t.shapeErr = key + ": lists no target - it takes one entry, starting with - id: <a name for this target>"

			return nil

		case 1:
			t.keyErrs = unknownKeys(key, value.Content[0], reflect.TypeOf(entry).Elem())

			return value.Content[0].Decode(entry)
		}

		t.shapeErr = fmt.Sprintf("%s: lists %d targets - a run captures one; run the agent once for each",
			key, len(value.Content))

		return nil
	}

	return fmt.Errorf("line %d: %s: is a list of targets, not a %s", value.Line, key, value.Tag)
}

// decodeErr is why the block was not read as written.
func (t *DBTarget) decodeErr() error {
	if t.shapeErr != "" {
		return errors.New(t.shapeErr)
	}

	return errors.Join(t.keyErrs...)
}

// unknownKeys refuses keys no field of typ takes, recursing into structs and <<
// merges. Decoding drops them silently, so a misspelled verifyServerCertificate
// would run unverified.
func unknownKeys(path string, node *yaml.Node, typ reflect.Type) []error {
	node = unaliased(node)
	if node.Kind != yaml.MappingNode {
		return nil
	}

	names, fields := yamlFields(typ)
	block := path[strings.LastIndex(path, ".")+1:]

	var errs []error

	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]

		if key.Value == "<<" && key.ShortTag() == "!!merge" {
			merged := []*yaml.Node{value}
			if list := unaliased(value); list.Kind == yaml.SequenceNode {
				merged = list.Content
			}

			for _, m := range merged {
				errs = append(errs, unknownKeys(path, m, typ)...)
			}

			continue
		}

		field, ok := fields[key.Value]
		if !ok {
			errs = append(errs, fmt.Errorf("%s.%s (line %d) is not a key %s: takes - its keys are %s",
				path, key.Value, key.Line, block, strings.Join(names, ", ")))

			continue
		}

		if field.Kind() == reflect.Struct {
			errs = append(errs, unknownKeys(path+"."+key.Value, value, field)...)
		}
	}

	return errs
}

// yamlFields are the keys a struct takes, as yaml.v3 names them, with their types.
func yamlFields(typ reflect.Type) ([]string, map[string]reflect.Type) {
	var names []string

	fields := map[string]reflect.Type{}

	var walk func(reflect.Type)

	walk = func(typ reflect.Type) {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}

			name, flags, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}

			fieldType := f.Type
			if fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}

			if slices.Contains(strings.Split(flags, ","), "inline") {
				walk(fieldType)

				continue
			}

			if name == "" {
				name = strings.ToLower(f.Name)
			}

			names = append(names, name)
			fields[name] = fieldType
		}
	}

	walk(typ)

	return names, fields
}

// unaliased is the node an alias stands for, or node itself.
func unaliased(node *yaml.Node) *yaml.Node {
	for node.Kind == yaml.AliasNode && node.Alias != nil {
		node = node.Alias
	}

	return node
}

// targetString is the block's fields for String, password redacted.
func (t *DBTarget) targetString() string {
	password := `""`
	if t.Password != "" {
		password = "<redacted>"
	}

	// Non-nil after Validate; this covers a block rendered before it.
	window := "(unset, required)"
	if t.CaptureDuration != nil {
		window = t.CaptureDuration.String()
	}

	frequency := fmt.Sprintf("(unset, defaults to %s)", DefaultFrequency)
	if t.Frequency != nil {
		frequency = t.Frequency.String()
	}

	caFile, serverName := "", ""
	if t.TLS != nil {
		caFile, serverName = t.TLS.CAFile, t.TLS.ServerName
	}

	return fmt.Sprintf(
		"id=%q host=%q port=%d database=%q username=%q password=%s "+
			"tls.enabled=%t tls.verifyServerCertificate=%t tls.caFile=%q tls.serverName=%q "+
			"captureDuration=%s frequency=%s explain=%s agentOnDbHost=%t",
		t.ID, t.Host, t.Port, t.Database, t.Username, password,
		t.TLSEnabled(), t.TLSVerified(), caFile, serverName,
		window, frequency, t.ExplainMode(), t.AgentOnDBHost,
	)
}

// trim normalizes the free-text fields in place; never the password.
func (t *DBTarget) trim() {
	t.ID = strings.TrimSpace(t.ID)
	t.Host = strings.TrimSpace(t.Host)
	t.Database = strings.TrimSpace(t.Database)
	t.Username = strings.TrimSpace(t.Username)

	if t.TLS != nil {
		t.TLS.CAFile = strings.TrimSpace(t.TLS.CAFile)
		t.TLS.ServerName = strings.TrimSpace(t.TLS.ServerName)
	}
	t.Explain = strings.ToLower(strings.TrimSpace(t.Explain))
}

// isZero reports whether the block names no target (window fields ignored).
func (t *DBTarget) isZero() bool {
	return t.ID == "" &&
		t.Host == "" &&
		t.Port == 0 &&
		t.Database == "" &&
		t.Username == "" &&
		t.Password == "" &&
		t.TLS == nil
}

// emptyBlockError is the error for a block naming no target.
func emptyBlockError(key string) error {
	return fmt.Errorf("%s block is present but empty or has no recognised keys "+
		"(valid keys: id, host, port, database, username, password, tls, captureDuration, "+
		"frequency, explain, agentOnDbHost)", key)
}

// validateWindow checks and clamps captureDuration and frequency.
func (t *DBTarget) validateWindow(key, fastFiles string) (warnings []string, errs []error) {
	// Over-ceiling clamps and warns (partial intent); missing or non-positive is rejected outright.
	switch {
	case t.CaptureDuration == nil:
		errs = append(errs, fmt.Errorf(
			"%s.captureDuration is required - how long to capture, for example 2m (at most %s)",
			key, MaxCaptureDuration))

	case t.CaptureDuration.Duration() <= 0:
		errs = append(errs, fmt.Errorf(
			"%s.captureDuration is %s - it must be positive (at most %s)",
			key, t.CaptureDuration, MaxCaptureDuration))

	case t.CaptureDuration.Duration() > MaxCaptureDuration:
		warnings = append(warnings, fmt.Sprintf(
			"%s.captureDuration %s exceeds the %s maximum - capturing for %s instead. "+
				"The window is a load commitment against a shared database.",
			key, t.CaptureDuration, MaxCaptureDuration, MaxCaptureDuration))

		t.CaptureDuration = newDuration(MaxCaptureDuration)
	}

	// Under-floor clamps and warns, as the ceiling does above; non-positive is rejected.
	frequencyDefaulted := false

	switch {
	case t.Frequency == nil:
		t.Frequency = newDuration(DefaultFrequency)
		frequencyDefaulted = true

	case t.Frequency.Duration() <= 0:
		errs = append(errs, fmt.Errorf(
			"%s.frequency is %s - it must be positive (omit the key for the %s default)",
			key, t.Frequency, DefaultFrequency))

	case t.Frequency.Duration() < MinFrequency:
		warnings = append(warnings, fmt.Sprintf(
			"%s.frequency %s is below the %s minimum - sampling every %s instead. "+
				"A sample's statements are bounded at %s, so a faster cadence would let one slow "+
				"sample outrun the tick behind it.",
			key, t.Frequency, MinFrequency, MinFrequency, MinFrequency))

		t.Frequency = newDuration(MinFrequency)
	}

	// Warn: a short window at the default frequency would be two samples of most files.
	if t.CaptureDuration != nil && t.Frequency.Duration() > 0 && t.CaptureDuration.Duration() > 0 &&
		t.Frequency.Duration() >= t.CaptureDuration.Duration() {
		warnings = append(warnings, t.bookendWarning(key, fastFiles, frequencyDefaulted))
	}

	return warnings, errs
}

// bookendWarning names the files left with only opening and closing samples.
func (t *DBTarget) bookendWarning(key, fastFiles string, frequencyDefaulted bool) string {
	window := t.CaptureDuration.Duration()
	fast := FastFrequency(t.Frequency.Duration())

	consequence := "normal-speed and expensive-speed files will produce only the opening and " +
		"closing samples, no samples in between"
	if fast < window {
		consequence += fmt.Sprintf("; fast-speed files (%s) are unaffected, "+
			"sampled every %s", fastFiles, fast)
	}

	if frequencyDefaulted {
		return fmt.Sprintf("%s.frequency is unset and defaults to %s, which meets or exceeds "+
			"%s.captureDuration (%s) - %s. Set %s.frequency (for example 30s) to sample "+
			"the normal-speed files within the window.",
			key, DefaultFrequency, key, t.CaptureDuration, consequence, key)
	}

	return fmt.Sprintf("%s.frequency (%s) meets or exceeds %s.captureDuration (%s) - %s.",
		key, t.Frequency, key, t.CaptureDuration, consequence)
}

// validateIdentity requires the target's name, address and user, and a port in range.
func (t *DBTarget) validateIdentity(key string) (errs []error) {
	if t.ID == "" {
		errs = append(errs, fmt.Errorf("%s.id is required - a name for this target, "+
			"for example orders-primary; every sample block carries it as target_id", key))
	}
	if t.Host == "" {
		errs = append(errs, fmt.Errorf("%s.host is required", key))
	}
	if t.Username == "" {
		errs = append(errs, fmt.Errorf("%s.username is required", key))
	}

	if t.Port < 1 || t.Port > 65535 {
		errs = append(errs, fmt.Errorf("%s.port %d is out of range (1-65535)", key, t.Port))
	}

	return errs
}

// validateTLS fills in defaults and refuses contradictory combinations.
func (t *DBTarget) validateTLS(key string) (warnings []string, errs []error) {
	if t.TLS == nil {
		t.TLS = &TargetTLS{}
	}

	enabled, verified := t.TLSEnabled(), t.TLSVerified()
	verifyAsked := t.TLS.VerifyServerCertificate != nil && *t.TLS.VerifyServerCertificate

	switch {
	case !enabled && verifyAsked:
		errs = append(errs, fmt.Errorf("%s.tls.verifyServerCertificate is true but "+
			"tls.enabled is false - a certificate can only be checked over TLS", key))

	case !enabled && (t.TLS.CAFile != "" || t.TLS.ServerName != ""):
		errs = append(errs, fmt.Errorf("%s.tls.caFile and tls.serverName have no effect "+
			"when tls.enabled is false - remove them, or enable TLS", key))

	case !enabled:
		warnings = append(warnings, key+".tls.enabled=false - the connection will not be "+
			"encrypted; credentials and captured query text would cross the network in plaintext.")

	case !verified && t.TLS.CAFile != "":
		errs = append(errs, fmt.Errorf("%s.tls.caFile is set but "+
			"tls.verifyServerCertificate is not - the CA is used only to verify the server's "+
			"certificate; set verifyServerCertificate: true, or remove caFile", key))
	}

	t.TLS.Enabled = &enabled
	t.TLS.VerifyServerCertificate = &verified

	return warnings, errs
}

// agentOnDBHostWarning is said whenever the declaration is made.
func agentOnDBHostWarning(key string) string {
	return key + ".agentOnDbHost=true - this machine's process list, " +
		"connection table, kernel messages and kernel settings will be captured and filed " +
		"under the database whenever the run cannot establish for itself that the two are " +
		"the same machine. A run that establishes they are not still skips them."
}

// expandPassword resolves ${NAME} in the password from the environment.
func (t *DBTarget) expandPassword(key string) []error {
	if t.Password == "" {
		return nil
	}

	var errs []error

	reported := map[string]bool{}

	t.Password = envRef.ReplaceAllStringFunc(t.Password, func(ref string) string {
		name := ref[2 : len(ref)-1]

		value, ok := os.LookupEnv(name)
		switch {
		case !ok:
			if !reported[name] {
				reported[name] = true
				errs = append(errs, fmt.Errorf(
					"%s.password references ${%s}, which is not set in the environment", key, name))
			}
			return ref
		case value == "":
			if !reported[name] {
				reported[name] = true
				errs = append(errs, fmt.Errorf(
					"%s.password references ${%s}, which is set but empty", key, name))
			}
			return ref
		default:
			return value
		}
	})

	return errs
}

func newDuration(d time.Duration) *Duration {
	wrapped := Duration(d)
	return &wrapped
}

// ErrBothDatabaseTargets refuses a file naming two engines: a run takes one target.
var ErrBothDatabaseTargets = errors.New("postgres: and mysql: are both configured - a run captures " +
	"one database target; keep one block, and run the agent once for each database")

// DBTargetConfigured reports whether a database block is configured.
func (o *Options) DBTargetConfigured() bool {
	return o.Postgres.IsConfigured() || o.MySQL.IsConfigured()
}

// DBTargetKey is the configured block's key, empty when there is none.
func (o *Options) DBTargetKey() string {
	switch {
	case o.Postgres.IsConfigured():
		return "postgres"

	case o.MySQL.IsConfigured():
		return "mysql"
	}

	return ""
}
