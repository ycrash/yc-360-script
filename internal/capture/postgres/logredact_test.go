package postgres

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedactContextReplacesSQLAndDataAndKeepsNames(t *testing.T) {
	for _, tt := range []struct {
		name     string
		context  string
		want     string
		redacted int
	}{
		{
			name:    "a tuple and a relation",
			context: `while updating tuple (0,1) in relation "yc_dl"`,
			want:    `while updating tuple (0,1) in relation "yc_dl"`,
		},
		{
			name:    "a function frame names its statement type, not its text",
			context: "PL/pgSQL function inline_code_block line 1 at RAISE",
			want:    "PL/pgSQL function inline_code_block line 1 at RAISE",
		},
		{
			// measured on postgres:18, 2026-09-27
			name:     "the SQL a function ran",
			context:  "SQL statement \"INSERT INTO p VALUES (1)\"\nPL/pgSQL function inline_code_block line 1 at SQL statement",
			want:     "SQL statement \"<redacted>\"\nPL/pgSQL function inline_code_block line 1 at SQL statement",
			redacted: 1,
		},
		{
			name: "SQL over several lines, up to the next frame",
			context: "SQL statement \"UPDATE t\n   SET note = 'a \"quoted\" word'\n WHERE id = 1\"\n" +
				"PL/pgSQL function f() line 3 at SQL statement",
			want:     "SQL statement \"<redacted>\"\nPL/pgSQL function f() line 3 at SQL statement",
			redacted: 1,
		},
		{
			name:     "an expression PL/pgSQL evaluated",
			context:  "SQL expression \"x > 'secret'\"\nPL/pgSQL function f() line 4 at IF",
			want:     "SQL expression \"<redacted>\"\nPL/pgSQL function f() line 4 at IF",
			redacted: 1,
		},
		{
			name:     "an assignment PL/pgSQL made",
			context:  "PL/pgSQL assignment \"total := 99.50\"\nPL/pgSQL function f() line 5 at assignment",
			want:     "PL/pgSQL assignment \"<redacted>\"\nPL/pgSQL function f() line 5 at assignment",
			redacted: 1,
		},
		{
			// measured on postgres:18, 2026-09-27
			name:     "a COPY column's value",
			context:  `COPY c, line 2, column id: "x secret"`,
			want:     `COPY c, line 2, column id: "<redacted>"`,
			redacted: 1,
		},
		{
			// measured on postgres:18, 2026-09-27; the line's own TABs are in it
			name:     "a COPY line of input",
			context:  "COPY c, line 2: \"11\t1\tbad\t-7\"",
			want:     `COPY c, line 2: "<redacted>"`,
			redacted: 1,
		},
		{
			name:    "a COPY column with no value",
			context: "COPY c, line 2, column note: null input",
			want:    "COPY c, line 2, column note: null input",
		},
		{
			// measured on postgres:18, 2026-09-27
			name:     "the json parser's input",
			context:  `JSON data, line 1: {"card": 4111 secret...`,
			want:     `JSON data, line 1: <redacted>`,
			redacted: 1,
		},
		{
			// measured on postgres:18, 2026-09-27
			name:     "a bind value",
			context:  "unnamed portal parameter $1 = 'bind secret'",
			want:     "unnamed portal parameter $1 = '<redacted>'",
			redacted: 1,
		},
		{
			// measured on postgres:18, 2026-09-27
			name:     "a portal's bind values",
			context:  "unnamed portal with parameters: $1 = '5', $2 = 'param secret'",
			want:     "unnamed portal with parameters: $1 = '<redacted>', $2 = '<redacted>'",
			redacted: 2,
		},
		{
			name:     "a value holding a quote and what looks like the next parameter",
			context:  `portal "C_1" with parameters: $1 = 'it''s, $2 = no', $2 = NULL`,
			want:     `portal "C_1" with parameters: $1 = '<redacted>', $2 = NULL`,
			redacted: 1,
		},
		{
			name:     "a frame of no known shape after a replaced one is taken as its text",
			context:  "SQL statement \"SELECT 1\"\nsomething no frame opens with",
			want:     "SQL statement \"<redacted>\"",
			redacted: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, redacted := redactContext(tt.context)

			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.redacted, redacted)
		})
	}
}

func TestRedactDeadlockReport(t *testing.T) {
	waits := "Process 112 waits for ShareLock on transaction 754; blocked by process 105.\n" +
		"Process 105 waits for ShareLock on transaction 755; blocked by process 112."

	got, redacted := redactDeadlockReport(waits)
	assert.Equal(t, waits, got, "a report of lock waits alone carries no statement")
	assert.Zero(t, redacted)

	got, redacted = redactDeadlockReport(waits + "\nProcess 112: UPDATE t\n   SET v = 'a'\n WHERE id = 2;\nProcess 105: SELECT 1")
	assert.Equal(t, waits+"\nProcess 112: <redacted>\nProcess 105: <redacted>", got,
		"a statement's lines go with it, up to the next process")
	assert.Equal(t, 2, redacted)

	translated := "Prozess 112 wartet auf ShareLock auf Transaktion 754; blockiert von Prozess 105.\n" +
		"Prozess 105 wartet auf ShareLock auf Transaktion 755; blockiert von Prozess 112.\n" +
		"Prozess 112: UPDATE yc_dl SET v=2 WHERE id=2;\n" +
		"Prozess 105: UPDATE yc_dl SET v=1 WHERE id=1;"

	got, redacted = redactDeadlockReport(translated)
	assert.Equal(t, "<redacted>", got,
		"a report in no shape the agent reads hides everything, the statements with it")
	assert.Equal(t, 1, redacted)
}

func TestLogRedactionStderrFields(t *testing.T) {
	r := &logRedaction{}

	for _, tt := range []struct {
		name     string
		event    string
		want     string
		redacted int
	}{
		{
			name:  "nothing to replace comes back as it was",
			event: "2026-09-27 01:49:46.933 UTC [12402] FATAL:  database \"yc_no_such_database\" does not exist\n",
			want:  "2026-09-27 01:49:46.933 UTC [12402] FATAL:  database \"yc_no_such_database\" does not exist\n",
		},
		{
			// measured on postgres:18, 2026-09-27: %l numbers every line of an entry
			name: "each line numbered apart by %l",
			event: "2026-09-27 04:37:42.252 UTC [53174] 1 ERROR:  yc prefix probe\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 2 DETAIL:  probe detail\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 3 HINT:  probe hint\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 4 CONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 5 STATEMENT:  DO $$ BEGIN RAISE EXCEPTION 'yc prefix probe'; END $$\n",
			want: "2026-09-27 04:37:42.252 UTC [53174] 1 ERROR:  yc prefix probe\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 2 DETAIL:  probe detail\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 3 HINT:  probe hint\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 4 CONTEXT:  PL/pgSQL function inline_code_block line 1 at RAISE\n" +
				"2026-09-27 04:37:42.252 UTC [53174] 5 STATEMENT:  <redacted>\n",
			redacted: 1,
		},
		{
			name:     "a Windows line ending is kept",
			event:    "ERROR:  boom\r\nSTATEMENT:  SELECT 'a'\r\n\tFROM t\r\n",
			want:     "ERROR:  boom\r\nSTATEMENT:  <redacted>\r\n",
			redacted: 1,
		},
		{
			name: "a stray line goes with the field before it",
			event: "2026-08-15 10:00:34.543 UTC [25666] ERROR:  boom\n" +
				"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  SELECT 'a'\n" +
				"a line from nowhere that names no keyword\n",
			want: "2026-08-15 10:00:34.543 UTC [25666] ERROR:  boom\n" +
				"2026-08-15 10:00:34.543 UTC [25666] STATEMENT:  <redacted>\n",
			redacted: 1,
		},
		{
			name: "a replaced field that ran over lines is written on their lines, TAB first",
			event: "ERROR:  boom\n" +
				"CONTEXT:  SQL statement \"SELECT 'a'\n\tFROM t\"\n" +
				"\tPL/pgSQL function f() line 3 at SQL statement\n",
			want: "ERROR:  boom\n" +
				"CONTEXT:  SQL statement \"<redacted>\"\n" +
				"\tPL/pgSQL function f() line 3 at SQL statement\n",
			redacted: 1,
		},
		{
			name:     "an internal query is replaced whole",
			event:    "ERROR:  syntax error at or near \"SELEC\" at character 1\nQUERY:  SELEC 'exec secret'\n",
			want:     "ERROR:  syntax error at or near \"SELEC\" at character 1\nQUERY:  <redacted>\n",
			redacted: 1,
		},
		{
			name:     "an event held back before its end was proven has no last newline",
			event:    "ERROR:  boom\nSTATEMENT:  SELECT 'a'",
			want:     "ERROR:  boom\nSTATEMENT:  <redacted>",
			redacted: 1,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, redacted := r.event([]byte(tt.event), logFormatStderr)

			assert.Equal(t, tt.want, string(got))
			assert.Equal(t, tt.redacted, redacted)
		})
	}
}

func TestLogRedactionCSVRecord(t *testing.T) {
	r := &logRedaction{}

	got, redacted := r.event([]byte(measuredMissingDatabaseCSV), logFormatCSV)
	assert.Equal(t, measuredMissingDatabaseCSV, string(got), "unquoted empty columns are NULL and stay")
	assert.Zero(t, redacted)

	appended := strings.TrimSuffix(measuredTimeoutBesideCSV, "\n") + `,"a column a later version appended"` + "\n"

	got, redacted = r.event([]byte(appended), logFormatCSV)
	assert.Equal(t, strings.Replace(appended, `"SELECT pg_sleep(2);"`, `"<redacted>"`, 1), string(got))
	assert.Equal(t, 1, redacted)

	got, redacted = r.event([]byte(`2026-09-27 01:49:46.564 UTC,"postgres","an open quote`+"\n"), logFormatCSV)
	assert.Equal(t, "<redacted>\n", string(got), "a record the agent cannot split is written as nothing")
	assert.Equal(t, 1, redacted)
}

func TestLogRedactionJSONRecord(t *testing.T) {
	r := &logRedaction{}

	escaped := `{"pid":1,"error_severity":"ERROR","message":"café \"quoted\"",` +
		`"context":"while updating tuple (0,1) in relation \"yc_` + string([]byte{0xff}) + `\"",` +
		`"statement":"SELECT '\\', E'\t', '\u0001'","nested":{"statement":"SELECT 2"},"query_id":0}` + "\n"

	got, redacted := r.event([]byte(escaped), logFormatJSON)
	assert.Equal(t,
		strings.Replace(escaped, `"statement":"SELECT '\\', E'\t', '\u0001'"`, `"statement":"<redacted>"`, 1),
		string(got),
		"only the top-level statement is re-encoded; the relation's name keeps its bytes")
	assert.Equal(t, 1, redacted)

	got, redacted = r.event([]byte(`{"message":"cut short`+"\n"), logFormatJSON)
	assert.Equal(t, "<redacted>\n", string(got), "a line the agent cannot split is written as nothing")
	assert.Equal(t, 1, redacted)
}

func TestEscapeJSONIsJsonlogsEncoding(t *testing.T) {
	for _, value := range []string{
		`plain`,
		`a \"quote\" and a \\ backslash`,
		`\b\f\n\r\t`,
		`\u0001\u001f`,
		`café, and bytes ` + string([]byte{0xff, 0xfe}),
	} {
		assert.Equal(t, `"`+value+`"`, escapeJSON(unescapeJSON([]byte(value))),
			"what the server escaped reads back and is escaped again the same")
	}

	assert.Equal(t, "\U0001F600", unescapeJSON([]byte(`😀`)), "a surrogate pair is one character")
}

func TestLogRedactionReEncodesOnlyTheFieldsItChanged(t *testing.T) {
	r := &logRedaction{deadlockReport: true}

	t.Run("csvlog", func(t *testing.T) {
		before := readCSVRecord(t, measuredDeadlockInFunctionCSV)

		got, _ := r.event([]byte(measuredDeadlockInFunctionCSV), logFormatCSV)
		after := readCSVRecord(t, string(got))

		require.Len(t, after, len(before))

		for i := range before {
			switch i {
			case csvDetailIndex, csvContextIndex, csvStatementIndex:
				assert.NotEqual(t, before[i], after[i], "column %d", i)
			default:
				assert.Equal(t, before[i], after[i], "column %d", i)
			}
		}
	})

	t.Run("jsonlog", func(t *testing.T) {
		var before, after map[string]any

		require.NoError(t, json.Unmarshal([]byte(measuredDeadlockInFunctionJSON), &before))

		got, _ := r.event([]byte(measuredDeadlockInFunctionJSON), logFormatJSON)
		require.NoError(t, json.Unmarshal(got, &after))

		require.Len(t, after, len(before))

		for key := range before {
			switch key {
			case "detail", "context", "statement":
				assert.NotEqual(t, before[key], after[key], key)
			default:
				assert.Equal(t, before[key], after[key], key)
			}
		}
	})
}

func readCSVRecord(t *testing.T, record string) []string {
	t.Helper()

	reader := csv.NewReader(bytes.NewReader([]byte(record)))
	reader.FieldsPerRecord = -1

	fields, err := reader.Read()
	require.NoError(t, err)

	return fields
}
