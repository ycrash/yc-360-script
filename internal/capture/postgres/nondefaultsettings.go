package postgres

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// nonDefaultSettingsColumns is six columns. name is the key across samples: it
// is unique in pg_settings.
var nonDefaultSettingsColumns = []string{
	"name",
	"setting",
	"unit",
	"source",
	"context",
	"short_desc",
}

// nonDefaultSettingsSQL is every setting not at its built-in default, as this
// session sees it. The agent's own startup settings are among them, as
// source=client. Settings a role may not read are absent rather than NULL:
// shared_preload_libraries, for one, needs pg_read_all_settings.
const nonDefaultSettingsSQL = `SELECT name, setting, unit, source, context, short_desc
FROM pg_catalog.pg_settings
WHERE source != 'default' AND source != 'override'
ORDER BY name`

// NonDefaultSettings captures the settings changed from their defaults every
// sample, with any password in a setting's value replaced.
type NonDefaultSettings struct {
	// Interval is the cadence, the run's expensive speed. Zero is the bookend alone.
	Interval time.Duration
}

func (n NonDefaultSettings) Artifact() Artifact {
	return Artifact{
		Name:     "pg_nondefault_settings",
		FileName: "pg_nondefault_settings.txt",

		// A database's and a role's own settings apply only to sessions in that
		// database or as that role.
		Scope:      "database",
		Schedule:   Periodic(n.Interval),
		Connection: ConnectionExpensive,
		Version:    sampleHeaderVersion,

		// One statement, not DefaultSampleBudget's two: Periodic's last sample is
		// the close, and the connection's closing tick is sized from this.
		SampleBudget: StatementTimeout,
	}
}

// Sample runs the one statement and writes one block. A statement that fails
// errors and writes nothing, and the window writes the stub.
func (n NonDefaultSettings) Sample(ctx context.Context, q RowQuerier, w io.Writer, s SampleContext) error {
	start := s.now()

	rows, err := readNonDefaultSettings(ctx, q)
	if err != nil {
		return err
	}

	reads := span{start, s.now()}

	cells, redacted := nonDefaultSettingsCells(rows)

	// Buffered so a write failure never leaves a half-written body.
	var block bytes.Buffer

	// Named for the view read; the window's own blocks name the artifact.
	if err := writeSampleHeader(&block, n.Artifact(), s, sampleHeader{
		source: "pg_settings",
		reads:  reads,
		status: statusOK,
		rows:   len(rows),
		fields: []headerField{{"redacted", strconv.Itoa(redacted)}},
	}); err != nil {
		return err
	}

	if err := writeRows(&block, nonDefaultSettingsColumns, cells); err != nil {
		return err
	}

	_, err = w.Write(block.Bytes())

	return err
}

// nonDefaultSettingRow is one setting. Every column but name is a pointer: unit
// is NULL for a setting without one, and an empty cell is not the same as "".
type nonDefaultSettingRow struct {
	name      string
	setting   *string
	unit      *string
	source    *string
	context   *string
	shortDesc *string
}

func readNonDefaultSettings(ctx context.Context, q RowQuerier) ([]nonDefaultSettingRow, error) {
	stmtCtx, cancel := statementContext(ctx)
	defer cancel()

	rows, err := q.Query(stmtCtx, nonDefaultSettingsSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var collected []nonDefaultSettingRow

	for rows.Next() {
		var row nonDefaultSettingRow

		if err := rows.Scan(&row.name, &row.setting, &row.unit, &row.source, &row.context, &row.shortDesc); err != nil {
			return nil, err
		}

		collected = append(collected, row)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return collected, nil
}

// nonDefaultSettingsCells redacts every setting's value, not only
// primary_conninfo's: any string setting can hold a connection string. It
// returns how many passwords it replaced.
func nonDefaultSettingsCells(rows []nonDefaultSettingRow) ([][]string, int) {
	cells := make([][]string, len(rows))
	redacted := 0

	for i, row := range rows {
		setting, n := redactPasswords(text(row.setting))
		redacted += n

		cells[i] = []string{row.name, setting, text(row.unit), text(row.source), text(row.context), text(row.shortDesc)}
	}

	return cells, redacted
}

// redactedValue is what a password becomes, as in Target's String.
const redactedValue = "<redacted>"

// connectionURI is a libpq URI. It runs to the next whitespace: a URI can't hold
// a raw space.
var connectionURI = regexp.MustCompile(`(?i)postgres(?:ql)?://[^ \t\n\v\f\r]*`)

// passwordKeyword is a keyword ending in "password" (password, sslpassword, a
// shell's PGPASSWORD) and its "=", with the spaces libpq allows either side.
var passwordKeyword = regexp.MustCompile(`(?i)password[ \t\n\v\f\r]*=[ \t\n\v\f\r]*`)

// redactPasswords replaces the passwords in a connection string, in either of
// libpq's forms, wherever it sits in s: at least what libpq would read as the
// password. Other shapes of secret, such as a token in a shell command, pass
// through.
func redactPasswords(s string) (string, int) {
	var out strings.Builder

	redacted := 0

	for {
		uri := connectionURI.FindStringIndex(s)
		keyword := passwordKeyword.FindStringIndex(s)

		switch {
		case keyword != nil && (uri == nil || keyword[0] < uri[0]):
			end := keyword[1] + keywordValueLength(s[keyword[1]:])

			out.WriteString(s[:keyword[1]])

			if end > keyword[1] {
				out.WriteString(redactedValue)
				redacted++
			}

			s = s[end:]

		case uri != nil:
			hidden, n := redactURIPasswords(s[uri[0]:uri[1]])

			out.WriteString(s[:uri[0]])
			out.WriteString(hidden)
			redacted += n

			s = s[uri[1]:]

		default:
			out.WriteString(s)

			return out.String(), redacted
		}
	}
}

// keywordValueLength is how much of s libpq reads as one keyword's value: a
// single-quoted string, or up to the next whitespace, a backslash escaping the
// character after it in both. An unterminated quote runs to the end.
func keywordValueLength(s string) int {
	quoted := strings.HasPrefix(s, "'")

	i := 0
	if quoted {
		i = 1
	}

	for i < len(s) {
		switch c := s[i]; {
		case c == '\\':
			i += 2

			continue

		case quoted && c == '\'':
			return i + 1

		case !quoted && strings.IndexByte(" \t\n\v\f\r", c) >= 0:
			return i
		}

		i++
	}

	return len(s)
}

// redactURIPasswords replaces the password in the two places a URI carries one:
// user:password@ before the hosts, and a parameter ending in "password".
// The user part ends at the last "@" before the path, so a password holding a
// raw "@" is hidden whole.
func redactURIPasswords(uri string) (string, int) {
	redacted := 0

	authority := strings.Index(uri, "://") + len("://")

	hosts := len(uri)
	if slash := strings.IndexByte(uri[authority:], '/'); slash >= 0 {
		hosts = authority + slash
	}

	if at := strings.LastIndexByte(uri[authority:hosts], '@'); at >= 0 {
		at += authority

		if colon := strings.IndexByte(uri[authority:at], ':'); colon >= 0 && authority+colon+1 < at {
			uri = uri[:authority+colon+1] + redactedValue + uri[at:]
			redacted++
		}
	}

	path, query, found := strings.Cut(uri, "?")
	if !found {
		return uri, redacted
	}

	params := strings.Split(query, "&")

	for i, param := range params {
		key, value, _ := strings.Cut(param, "=")

		if decoded, err := url.PathUnescape(key); err == nil {
			key = decoded
		}

		if value != "" && strings.HasSuffix(strings.ToLower(key), "password") {
			params[i] = param[:len(param)-len(value)] + redactedValue
			redacted++
		}
	}

	return path + "?" + strings.Join(params, "&"), redacted
}
