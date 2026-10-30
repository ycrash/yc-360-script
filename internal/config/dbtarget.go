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

// DBTarget is what every database block shares: the target, how to reach it and
// prove who is asking, and the window it is sampled over. Each engine's block
// embeds it inline, so its keys sit beside the engine's own in the file, and
// validates it in the same order, with its own key in every message.
type DBTarget struct {
	// ID names the target; every sample block carries it as target_id=. Required.
	ID string `yaml:"id"`

	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	Database string `yaml:"database"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`

	// TLS is how the connection is encrypted and checked. nil (key omitted) is
	// encrypted with the certificate not verified; Validate fills it in.
	TLS *TargetTLS `yaml:"tls"`

	// CaptureDuration is how long to capture. Required: nil (key omitted) and 0s are
	// configuration errors.
	CaptureDuration *Duration `yaml:"captureDuration"`

	// Frequency is how often the periodic artifacts are sampled, and the source of
	// the three speeds (FastFrequency, ExpensiveFrequency). Pointer: nil (key
	// omitted) takes the default; 0s is a configuration error. Whatever the value,
	// the opening and closing samples are always taken, so a frequency no shorter
	// than the window leaves exactly those two - and Validate says so.
	Frequency *Duration `yaml:"frequency"`

	// Explain selects which plan-capture tiers run. Empty (key omitted) captures no
	// plans: EXPLAIN carries a privilege and a load cost the other artifacts do not.
	Explain string `yaml:"explain"`

	// AgentOnDBHost declares that this machine runs the database, for the one case
	// the probe cannot reach: the database is down, so there is no backend to look
	// for - exactly when host readings matter most. A measurement always wins.
	AgentOnDBHost bool `yaml:"agentOnDbHost"`

	// shapeErr is why the block was not a list of one target, set when it is
	// decoded; Validate refuses it.
	shapeErr string

	// keyErrs are the entry's keys the block does not take, set when it is decoded;
	// Validate refuses them.
	keyErrs []error
}

// TargetTLS is the tls: block.
type TargetTLS struct {
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

const (
	// MaxCaptureDuration caps captureDuration: a load commitment against a shared
	// database. Two hours is the longest window supported; the host files stretch
	// with it, so a long window's netstat and ps readings are far apart.
	MaxCaptureDuration = 2 * time.Hour

	// DefaultFrequency is 5m. A short incident capture that wants samples between
	// the endpoints sets frequency itself (30s, for example), and one that does not
	// is warned.
	DefaultFrequency = 5 * time.Minute

	// MaxFastFrequency caps the fast speed, session and lock state: a blocking
	// episode shorter than one sampling interval can pass unseen.
	MaxFastFrequency = 15 * time.Second

	// MinExpensiveFrequency floors the expensive speed: whole-table reads and
	// settings, costly to take and slow to change.
	MinExpensiveFrequency = 5 * time.Minute

	// MinFrequency floors frequency. It equals each engine's per-statement
	// timeout, pinned by a test in internal/capture, so a maxed-out sample can
	// never outrun the tick behind it; below it the timeline could not catch up
	// under load.
	MinFrequency = 5 * time.Second

	// ExplainLogged captures only the plans the server itself logged - nothing is
	// submitted back to the database.
	ExplainLogged = "logged"

	// ExplainAll adds the estimated tiers, which submit EXPLAIN statements.
	ExplainAll = "all"

	// ExplainOff is what an omitted key reports as. It is not an accepted input:
	// presence is the switch, so turning the feature off means deleting the line.
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

// explainBooleans are what a human types instead of omitting the key; yaml.v3
// passes them through, where the generic error would not say to delete the line.
var explainBooleans = []string{"true", "false", "on", "off", "yes", "no"}

var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// TLSEnabled and TLSVerified read the block with its defaults, whether or not
// Validate has filled them in.
func (t *DBTarget) TLSEnabled() bool {
	return t.TLS == nil || t.TLS.Enabled == nil || *t.TLS.Enabled
}

func (t *DBTarget) TLSVerified() bool {
	return t.TLSEnabled() && t.TLS != nil && t.TLS.VerifyServerCertificate != nil && *t.TLS.VerifyServerCertificate
}

// ExplainMode is the run's plan-capture intent as a token: an accepted value, or
// ExplainOff for an omitted key. Each engine's metadata file records it.
func (t *DBTarget) ExplainMode() string {
	if t == nil || t.Explain == "" {
		return ExplainOff
	}

	return t.Explain
}

// decodeTargetList reads key: as a list of targets, decoding a list of one into
// entry, the engine's block as a type without UnmarshalYAML. Any other shape still
// decodes, with the reason in shapeErr, so that Validate can say what to write
// instead; so does an entry with keys the block does not take, in keyErrs.
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

// decodeErr is why the block was not read as written: its shape, or keys it does
// not take.
func (t *DBTarget) decodeErr() error {
	if t.shapeErr != "" {
		return errors.New(t.shapeErr)
	}

	return errors.Join(t.keyErrs...)
}

// unknownKeys refuses each key of the mapping node that no field of typ takes,
// naming it under path with its line, and looks inside a key whose field is a
// struct, as tls: is. Decoding drops such a key without a word, so a misspelled
// verifyServerCertificate: true would run unverified. A << merge brings its
// mapping's keys in, and they are checked the same way.
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

// yamlFields are the keys a struct takes, in its order, and each one's type with
// any pointer taken off: the yaml tag's name, or as yaml.v3 does without one, the
// lowercased field name. An inline field's keys sit beside its own.
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

// trim normalizes the free-text fields in place. The password is not trimmed: it
// must reach the driver byte-exact.
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

// isZero reports whether the block names no target. CaptureDuration and Frequency
// are excluded: a window or a cadence alone hasn't said what to capture, so that
// case gets the "empty" error.
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

// validateWindow checks captureDuration and frequency, clamping what is over the
// ceiling or under the floor, and warns when the frequency leaves only the
// bookends. fastFiles names the engine's fast-speed files for that warning.
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

	// The bookend is always taken, so this is a warning and not an error - but a
	// short window with the default frequency lands here, and a capture that was
	// never told a cadence would otherwise be silently two samples of most files.
	if t.CaptureDuration != nil && t.Frequency.Duration() > 0 && t.CaptureDuration.Duration() > 0 &&
		t.Frequency.Duration() >= t.CaptureDuration.Duration() {
		warnings = append(warnings, t.bookendWarning(key, fastFiles, frequencyDefaulted))
	}

	return warnings, errs
}

// bookendWarning says which files keep only the opening and closing samples when
// frequency meets or exceeds the window. The fast speed is capped below
// frequency, so session state still samples within a window longer than it.
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

// validateTLS fills in the block's defaults, so what the run uses is what it
// says, and refuses the combinations that could only be a mistake.
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

// agentOnDBHostWarning is said whenever the declaration is made: it widens what the
// run captures.
func agentOnDBHostWarning(key string) string {
	return key + ".agentOnDbHost=true - this machine's process list, " +
		"connection table, kernel messages and kernel settings will be captured and filed " +
		"under the database whenever the run cannot establish for itself that the two are " +
		"the same machine. A run that establishes they are not still skips them."
}

// expandPassword resolves every ${NAME} in the password from the environment, with
// one error per name that could not be resolved.
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

// DBTargetConfigured reports whether the options name a database target of any
// engine, which makes the run a database capture.
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
