package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func validMySQL() *MySQL {
	return &MySQL{DBTarget: DBTarget{
		ID:              "orders-primary",
		Host:            "db-prod-02.internal",
		Username:        "ycrash_monitor",
		CaptureDuration: newDuration(2 * time.Minute),
		Frequency:       newDuration(30 * time.Second),
	}}
}

func TestMySQLDefaultsThePortAndLeavesTheDatabaseOptional(t *testing.T) {
	m := validMySQL()

	warnings, err := m.Validate()
	require.NoError(t, err)

	assert.Empty(t, warnings, "no database is needed to connect, so none is defaulted or warned about")
	assert.Equal(t, DefaultMySQLPort, m.Port)
	assert.Empty(t, m.Database)
	assert.True(t, m.TLSEnabled())
	assert.False(t, m.TLSVerified())
}

func TestMySQLExplainTakesOnlyAll(t *testing.T) {
	m := validMySQL()
	m.Explain = "logged"

	_, err := m.Validate()
	assert.ErrorContains(t, err, `mysql.explain is "logged" - MySQL has no auto_explain`)

	m = validMySQL()
	m.Explain = "true"

	_, err = m.Validate()
	assert.ErrorContains(t, err, `mysql.explain is "true" - it takes "all"`)

	m = validMySQL()
	m.Explain = " ALL "

	warnings, err := m.Validate()
	require.NoError(t, err)
	assert.Equal(t, ExplainAll, m.ExplainMode())
	assert.Contains(t, warnings[0], "mysql.explain=all - each statement's query_sample_text")
}

func TestMySQLMessagesNameTheirOwnBlock(t *testing.T) {
	m := &MySQL{DBTarget: DBTarget{Port: 70000, TLS: &TargetTLS{Enabled: new(bool), CAFile: "/ca.pem"}}}

	_, err := m.Validate()

	for _, want := range []string{
		"mysql.captureDuration is required",
		"mysql.id is required",
		"mysql.host is required",
		"mysql.username is required",
		"mysql.port 70000 is out of range",
		"mysql.tls.caFile and tls.serverName have no effect",
	} {
		assert.ErrorContains(t, err, want)
	}

	assert.NotContains(t, err.Error(), "postgres")
}

func TestMySQLBookendWarningNamesItsFastFiles(t *testing.T) {
	m := validMySQL()
	m.Frequency = nil

	warnings, err := m.Validate()
	require.NoError(t, err)
	assert.Contains(t, warnings[0], "fast-speed files (my_sessions.txt, my_lock_waits.txt) are unaffected")
}

func TestMySQLIsAListOfOneTarget(t *testing.T) {
	var options Options

	require.NoError(t, yaml.Unmarshal([]byte("mysql:\n  - id: a\n    host: db\n    database: orders\n"), &options))
	require.NotNil(t, options.MySQL)
	assert.Equal(t, "db", options.MySQL.Host)
	assert.Equal(t, "orders", options.MySQL.Database, "the embedded base decodes inline")

	options = Options{}
	require.NoError(t, yaml.Unmarshal([]byte("mysql:\n  host: db\n"), &options))

	_, err := options.MySQL.Validate()
	assert.ErrorContains(t, err, "mysql: is a list of targets")
}

func TestMySQLRefusesAKeyItDoesNotTake(t *testing.T) {
	var options Options

	require.NoError(t, yaml.Unmarshal([]byte(`mysql:
  - id: a
    host: db
    username: u
    captureDuraton: 2m
    tls:
      verifyServerCertifcate: true
`), &options))

	_, err := options.MySQL.Validate()
	require.Error(t, err, "a dropped verifyServerCertificate: true would run unverified, without a word")
	assert.ErrorContains(t, err, "mysql.captureDuraton (line 5) is not a key mysql: takes - its keys are "+
		"id, host, port, database, username, password, tls, captureDuration, frequency, explain, agentOnDbHost")
	assert.ErrorContains(t, err, "mysql.tls.verifyServerCertifcate (line 7) is not a key tls: takes - its keys are "+
		"enabled, verifyServerCertificate, caFile, serverName")
}

func TestMySQLKeysMergedFromAnAnchorAreCheckedToo(t *testing.T) {
	var doc struct {
		Defaults map[string]any `yaml:"defaults"`
		Options  Options        `yaml:"options"`
	}

	require.NoError(t, yaml.Unmarshal([]byte(`defaults:
  tls: &tls
    verifyServerCertificate: true
  target: &target
    username: u
    captureDuration: 2m
options:
  mysql:
    - <<: *target
      id: a
      host: db
      tls: *tls
`), &doc))

	_, err := doc.Options.MySQL.Validate()
	require.NoError(t, err, "a merge key and an alias are YAML, not keys of the block")
	assert.True(t, doc.Options.MySQL.TLSVerified())

	doc.Options = Options{}
	require.NoError(t, yaml.Unmarshal([]byte(`defaults:
  target: &target
    username: u
    captureDuration: 2m
    tls: {verifyServerCertifcate: true}
options:
  mysql:
    - <<: *target
      id: a
      host: db
`), &doc))

	_, err = doc.Options.MySQL.Validate()
	assert.ErrorContains(t, err, "mysql.tls.verifyServerCertifcate (line 5) is not a key tls: takes")
}

func TestOneDatabaseTargetPerRun(t *testing.T) {
	options := Options{Postgres: &Postgres{}, MySQL: &MySQL{}}

	assert.True(t, options.DBTargetConfigured())
	assert.Equal(t, "postgres", options.DBTargetKey())

	options = Options{MySQL: &MySQL{}}
	assert.Equal(t, "mysql", options.DBTargetKey())

	assert.False(t, (&Options{}).DBTargetConfigured())
}

func TestMySQLNeverPrintsItsPassword(t *testing.T) {
	m := validMySQL()
	m.Password = "s3cret"

	for _, rendered := range []string{m.String(), m.GoString()} {
		assert.NotContains(t, rendered, "s3cret")
		assert.Contains(t, rendered, "password=<redacted>")
	}
}
