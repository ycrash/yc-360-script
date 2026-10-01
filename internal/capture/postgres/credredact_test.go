package postgres

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRedactCredentials(t *testing.T) {
	cases := []struct {
		name, in, out string
		redacted      int
	}{
		{"a role's password", "CREATE ROLE app LOGIN PASSWORD 'secret'",
			"CREATE ROLE app LOGIN PASSWORD '<redacted>'", 1},
		{"an encrypted password, as psql's \\password sends it", "ALTER USER app WITH ENCRYPTED PASSWORD 'SCRAM-SHA-256$4096:abc'",
			"ALTER USER app WITH ENCRYPTED PASSWORD '<redacted>'", 1},
		{"any case, and what follows stays", "alter role app password 'secret' valid until '2027-01-01'",
			"alter role app password '<redacted>' valid until '2027-01-01'", 1},
		{"a doubled quote inside", "ALTER ROLE app PASSWORD 'it''s secret'",
			"ALTER ROLE app PASSWORD '<redacted>'", 1},
		{"an escape string keeps its prefix", `ALTER ROLE app PASSWORD E'it\'s secret'`,
			"ALTER ROLE app PASSWORD E'<redacted>'", 1},
		{"a dollar-quoted password keeps its tags", "ALTER ROLE app PASSWORD $pw$secret$pw$",
			"ALTER ROLE app PASSWORD $pw$<redacted>$pw$", 1},
		{"a comment before the value", "ALTER ROLE app PASSWORD /* rotated */ 'secret'",
			"ALTER ROLE app PASSWORD /* rotated */ '<redacted>'", 1},
		{"no password is no change", "ALTER ROLE app PASSWORD NULL", "ALTER ROLE app PASSWORD NULL", 0},
		{"a user mapping's option", "CREATE USER MAPPING FOR bob SERVER remote OPTIONS (user 'bob', password 'secret')",
			"CREATE USER MAPPING FOR bob SERVER remote OPTIONS (user 'bob', password '<redacted>')", 1},
		{"a quoted option name", `ALTER USER MAPPING FOR bob SERVER remote OPTIONS (SET "password" 'secret')`,
			`ALTER USER MAPPING FOR bob SERVER remote OPTIONS (SET "password" '<redacted>')`, 1},
		{"a word ending in password", "OPTIONS (sslpassword 'key-pw')", "OPTIONS (sslpassword '<redacted>')", 1},
		{"a comparison or assignment", "UPDATE app_users SET password = 'secret' WHERE id = 7",
			"UPDATE app_users SET password = '<redacted>' WHERE id = 7", 1},
		{"a parameter is already no value", "UPDATE app_users SET password = $1 WHERE id = $2",
			"UPDATE app_users SET password = $1 WHERE id = $2", 0},
		{"a setting named for passwords is not one", "SET password_encryption = 'scram-sha-256'",
			"SET password_encryption = 'scram-sha-256'", 0},
		{"a column is not a value", "SELECT password FROM app_users WHERE name = 'bob'",
			"SELECT password FROM app_users WHERE name = 'bob'", 0},
		{"prose in a literal", "INSERT INTO notes VALUES ('my password is long')",
			"INSERT INTO notes VALUES ('my password is long')", 0},
		{"a connection string in a literal", "CREATE SUBSCRIPTION s CONNECTION 'host=pub password=secret dbname=d' PUBLICATION p",
			"CREATE SUBSCRIPTION s CONNECTION 'host=pub password=<redacted> dbname=d' PUBLICATION p", 1},
		{"a quoted connection-string value, its quotes doubled", "CREATE SUBSCRIPTION s CONNECTION 'host=pub password=''a b'' dbname=d' PUBLICATION p",
			"CREATE SUBSCRIPTION s CONNECTION 'host=pub password=<redacted> dbname=d' PUBLICATION p", 1},
		{"a shell variable in a literal", "COPY t FROM PROGRAM 'PGPASSWORD=secret psql -h h -c x'",
			"COPY t FROM PROGRAM 'PGPASSWORD=<redacted> psql -h h -c x'", 1},
		{"a uri in a literal", "SELECT dblink_connect('postgresql://u:secret@h/db?sslpassword=key-pw')",
			"SELECT dblink_connect('postgresql://u:<redacted>@h/db?sslpassword=<redacted>')", 2},
		{"a DO block's body", "DO $$ BEGIN CREATE ROLE r LOGIN PASSWORD 'secret'; END $$",
			"DO $$ BEGIN CREATE ROLE r LOGIN PASSWORD '<redacted>'; END $$", 1},
		{"a statement quoted inside a body", "DO $$ BEGIN EXECUTE 'CREATE ROLE r PASSWORD ''secret'''; END $$",
			"DO $$ BEGIN EXECUTE 'CREATE ROLE r PASSWORD ''<redacted>'''; END $$", 1},
		{"a literal the text cut off", "ALTER ROLE app PASSWORD 'secr",
			"ALTER ROLE app PASSWORD '<redacted>", 1},
		{"a connection string the text cut off", "SELECT dblink_connect('host=h password=secr",
			"SELECT dblink_connect('host=h password=<redacted>", 1},
		{"normalized text", "SELECT * FROM orders WHERE id = $1", "SELECT * FROM orders WHERE id = $1", 0},
		{"empty", "", "", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, redacted := redactCredentials(c.in)

			assert.Equal(t, c.out, out)
			assert.Equal(t, c.redacted, redacted)
		})
	}
}

func TestQueryCellRedactsWhatTheCapCutAndKeepsItsMarker(t *testing.T) {
	query := "ALTER ROLE app PASSWORD '" + strings.Repeat("s", DefaultMaxQueryText) + "'"

	cell, cut, redacted := queryCell(query)

	assert.True(t, cut)
	assert.Equal(t, 1, redacted, "a literal the cap split is still a password")
	assert.Equal(t, "ALTER ROLE app PASSWORD '<redacted>...", cell,
		"the cap's marker stays last, so a cut cell still says it was cut")
}

func TestQueryCellLeavesTextWithoutCredentialsAsCaptured(t *testing.T) {
	query := "SELECT note FROM orders WHERE note = 'a ''quoted'' value' -- password"

	cell, cut, redacted := queryCell(query)

	assert.Equal(t, query, cell)
	assert.False(t, cut)
	assert.Zero(t, redacted)
}
