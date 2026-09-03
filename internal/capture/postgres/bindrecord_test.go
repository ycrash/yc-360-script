package postgres

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every fixture below is byte-for-byte from the matrix containers, 2026-09-03, under
// log_min_duration_statement=0 with a parameterized statement sent over the extended
// protocol: postgres:14 for the lower-case label, postgres:18 for the capitalised one.

// measuredBindStderr is PostgreSQL 14's stderr record for two parameters, one of them
// carrying quotes, under the default log_line_prefix.
const measuredBindStderr = "2026-09-03 00:48:41.640 UTC [78498] LOG:  duration: 0.103 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE id = $1 AND status <> $2\n" +
	"2026-09-03 00:48:41.640 UTC [78498] DETAIL:  parameters: $1 = '42', $2 = 'it''s a ''quote'''\n"

// measuredBindStderrMultiline is a value with an embedded newline: the server writes
// one TAB after it, on both the statement and the parameter side.
const measuredBindStderrMultiline = "2026-09-03 00:48:51.424 UTC [78971] LOG:  duration: 0.118 ms  execute <unnamed>: SELECT count(*)\n" +
	"\tFROM yc_bloat_orders WHERE status = $1\n" +
	"2026-09-03 00:48:51.424 UTC [78971] DETAIL:  Parameters: $1 = 'line1\n" +
	"\tline2'\n"

// measuredBindStderrPrefixed is the same record under a log_line_prefix carrying %Q.
const measuredBindStderrPrefixed = "2026-09-03 00:48:45.641 UTC [78519] postgres@postgres [unknown] qid=-2760221630656037670 LOG:  duration: 0.023 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE id = $1 AND status <> $2\n" +
	"2026-09-03 00:48:45.641 UTC [78519] postgres@postgres [unknown] qid=-2760221630656037670 DETAIL:  parameters: $1 = '42', $2 = 'it''s a ''quote'''\n"

const measuredBindPrefixTemplate = "%m [%p] %q%u@%d %a qid=%Q "

// measuredBindCSV is the csvlog record: message and detail as fields, and the
// identifier as the 26th column.
const measuredBindCSV = `2026-09-03 00:48:57.744 UTC,"postgres","postgres",79000,"192.168.65.1:57810",6a98c3f9.13498,5,"SELECT",2026-09-03 00:48:57 UTC,45/211,0,LOG,00000,"duration: 0.041 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE id = $1 AND status <> $2","Parameters: $1 = '42', $2 = 'it''s a ''quote'''",,,,,,,,"","client backend",,3285802980598187581` + "\n"

// measuredBindCSVMultiline carries a real newline inside the quoted detail field.
const measuredBindCSVMultiline = `2026-09-03 00:48:47.975 UTC,"postgres","postgres",78528,"192.168.65.1:20516",6a98c3ef.132c0,11,"SELECT",2026-09-03 00:48:47 UTC,4/2419,0,LOG,00000,"duration: 0.113 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE status = $1","parameters: $1 = 'line1
line2'",,,,,,,,"","client backend",,-6450469992402841788` + "\n"

// measuredBindJSON is the jsonlog record; query_id is a number.
const measuredBindJSON = `{"timestamp":"2026-09-03 00:49:00.071 UTC","user":"postgres","dbname":"postgres","pid":79009,"remote_host":"192.168.65.1","remote_port":30078,"session_id":"6a98c3fc.134a1","line_num":5,"ps":"SELECT","session_start":"2026-09-03 00:49:00 UTC","vxid":"43/204","txid":0,"error_severity":"LOG","message":"duration: 0.018 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE id = $1 AND status <> $2","detail":"Parameters: $1 = '42', $2 = 'it''s a ''quote'''","backend_type":"client backend","query_id":3285802980598187581}` + "\n"

const measuredBindJSONMultiline = `{"timestamp":"2026-09-03 00:49:00.072 UTC","pid":79009,"error_severity":"LOG","message":"duration: 0.038 ms  execute <unnamed>: SELECT count(*) FROM yc_bloat_orders WHERE status = $1","detail":"Parameters: $1 = 'line1\nline2'","backend_type":"client backend","query_id":-3411363329778510676}` + "\n"

const measuredBindStatement = "SELECT count(*) FROM yc_bloat_orders WHERE id = $1 AND status <> $2"

func measuredBindValues() []bindValue {
	return []bindValue{{text: "42"}, {text: "it's a 'quote'"}}
}

// --- the parser --------------------------------------------------------------

func TestBindParametersParse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  string
		values []bindValue
		reason string
	}{
		{name: "one value", input: "$1 = '42'", values: []bindValue{{text: "42"}}},
		{name: "in order", input: "$1 = '42', $2 = 'x'", values: []bindValue{{text: "42"}, {text: "x"}}},
		{name: "out of order", input: "$2 = 'x', $1 = '42'", values: []bindValue{{text: "42"}, {text: "x"}}},
		{name: "quotes doubled", input: "$1 = 'it''s a ''quote'''", values: []bindValue{{text: "it's a 'quote'"}}},
		{name: "SQL NULL", input: "$1 = NULL", values: []bindValue{{null: true}}},
		{name: "the string NULL", input: "$1 = 'NULL'", values: []bindValue{{text: "NULL"}}},
		{name: "empty string", input: "$1 = ''", values: []bindValue{{text: ""}}},
		{name: "a comma inside a value", input: "$1 = 'a, $2 = b', $2 = 'c'",
			values: []bindValue{{text: "a, $2 = b"}, {text: "c"}}},
		{name: "a newline inside a value", input: "$1 = 'line1\nline2'", values: []bindValue{{text: "line1\nline2"}}},
		{name: "exactly at the cap is whole", input: "$1 = '12345678'", values: []bindValue{{text: "12345678"}}},

		{name: "clipped", input: "$1 = 'abcdefgh...'", reason: reasonBindTruncated},
		{name: "clipped on a character boundary", input: "$1 = 'éééé...'", reason: reasonBindTruncated},
		{name: "a genuine trailing ellipsis is indistinguishable", input: "$1 = 'ab...'", reason: reasonBindTruncated},
		{name: "clipped among whole values", input: "$1 = '42', $2 = 'abcdefgh...'", reason: reasonBindTruncated},

		{name: "a position missing", input: "$1 = '42', $3 = 'x'", reason: reasonBindMalformed},
		{name: "starting past one", input: "$2 = 'x'", reason: reasonBindMalformed},
		{name: "a position twice", input: "$1 = '42', $1 = 'x'", reason: reasonBindMalformed},
		{name: "unterminated", input: "$1 = 'abc", reason: reasonBindMalformed},
		{name: "unquoted", input: "$1 = 42", reason: reasonBindMalformed},
		{name: "not a position", input: "$x = '42'", reason: reasonBindMalformed},
		{name: "position zero", input: "$0 = '42'", reason: reasonBindMalformed},
		{name: "NULL with a tail", input: "$1 = NULLS", reason: reasonBindMalformed},
		{name: "junk between entries", input: "$1 = '42' $2 = 'x'", reason: reasonBindMalformed},
		{name: "a trailing separator", input: "$1 = '42', ", reason: reasonBindMalformed},
		{name: "nothing", input: "", reason: reasonBindMalformed},
		{name: "the tail's own truncation mark", input: "$1 = '42', $2 = 'ab...", reason: reasonBindMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values, reason := parseBindParameters(tc.input)

			assert.Equal(t, tc.reason, reason)
			assert.Equal(t, tc.values, values)
		})
	}
}

func TestRenderArgument(t *testing.T) {
	for _, tc := range []struct {
		value bindValue
		want  string
	}{
		{bindValue{null: true}, "NULL"},
		{bindValue{text: "42"}, "E'42'"},
		{bindValue{text: "NULL"}, "E'NULL'"},
		{bindValue{text: "it's"}, "E'it''s'"},
		{bindValue{text: `\x01`}, `E'\\x01'`},
		{bindValue{text: "a\nb"}, "E'a\nb'"},
		{bindValue{text: ""}, "E''"},
	} {
		assert.Equal(t, tc.want, renderArgument(tc.value),
			"quotes and backslashes doubled, everything else as the server printed it")
	}
}

// --- the stderr prefix -------------------------------------------------------

func TestCompileLinePrefix(t *testing.T) {
	measured := "2026-09-03 00:48:45.641 UTC [78519] postgres@postgres [unknown] qid=-2760221630656037670 "

	t.Run("the default prefix identifies nothing", func(t *testing.T) {
		assert.Empty(t, compileLinePrefix("%m [%p] ").queryID("2026-09-03 00:48:41.640 UTC [78498] "))
	})

	t.Run("%Q is the capture", func(t *testing.T) {
		assert.Equal(t, "-2760221630656037670",
			compileLinePrefix(measuredBindPrefixTemplate).queryID(measured))
	})

	t.Run("a prefix the template does not describe is no proof", func(t *testing.T) {
		assert.Empty(t, compileLinePrefix(measuredBindPrefixTemplate).queryID(
			"2026-09-03 00:48:41.640 UTC [78498] "))
	})

	t.Run("zero is the server's none", func(t *testing.T) {
		assert.Empty(t, compileLinePrefix("%Q ").queryID("0 "))
	})

	t.Run("padding and literal percent", func(t *testing.T) {
		prefix := compileLinePrefix("%5p|%-24Q|%%|")

		assert.Equal(t, "4242", prefix.queryID("  123|4242                    |%|"))
	})

	t.Run("%q contributes nothing on a session line", func(t *testing.T) {
		assert.Equal(t, "77", compileLinePrefix("%t %q[%Q] ").queryID("2026-09-03 00:48:41 UTC [77] "))
	})

	t.Run("a nil prefix identifies nothing", func(t *testing.T) {
		var prefix *linePrefix

		assert.Empty(t, prefix.queryID("anything"))
	})
}

// --- the three formats -------------------------------------------------------

func TestParseStderrEntry(t *testing.T) {
	t.Run("message and detail", func(t *testing.T) {
		entry, ok := parseStderrEntry([]byte(measuredBindStderr), compileLinePrefix("%m [%p] "))
		require.True(t, ok)

		assert.Equal(t, "duration: 0.103 ms  execute <unnamed>: "+measuredBindStatement, entry.message)
		assert.Equal(t, "parameters: $1 = '42', $2 = 'it''s a ''quote'''", entry.detail)
		assert.Empty(t, entry.queryID, "the default prefix carries none")
	})

	t.Run("continuation lines lose one TAB each", func(t *testing.T) {
		entry, ok := parseStderrEntry([]byte(measuredBindStderrMultiline), nil)
		require.True(t, ok)

		assert.Equal(t, "duration: 0.118 ms  execute <unnamed>: SELECT count(*)\nFROM yc_bloat_orders WHERE status = $1",
			entry.message)
		assert.Equal(t, "Parameters: $1 = 'line1\nline2'", entry.detail)
	})

	t.Run("%Q in the prefix is the identifier", func(t *testing.T) {
		entry, ok := parseStderrEntry([]byte(measuredBindStderrPrefixed),
			compileLinePrefix(measuredBindPrefixTemplate))
		require.True(t, ok)

		assert.Equal(t, "-2760221630656037670", entry.queryID)
		assert.Equal(t, "parameters: $1 = '42', $2 = 'it''s a ''quote'''", entry.detail)
	})

	t.Run("other secondary lines are not the detail", func(t *testing.T) {
		entry, ok := parseStderrEntry([]byte(
			"2026-09-03 00:48:41.640 UTC [78498] LOG:  duration: 0.103 ms  execute <unnamed>: SELECT 1\n"+
				"2026-09-03 00:48:41.640 UTC [78498] HINT:  none\n"+
				"\tstill the hint\n"+
				"2026-09-03 00:48:41.640 UTC [78498] DETAIL:  Parameters: $1 = 'x'\n"), nil)
		require.True(t, ok)

		assert.Equal(t, "duration: 0.103 ms  execute <unnamed>: SELECT 1", entry.message)
		assert.Equal(t, "Parameters: $1 = 'x'", entry.detail)
	})

	t.Run("no severity keyword is no entry", func(t *testing.T) {
		_, ok := parseStderrEntry([]byte("\tjust a continuation\n"), nil)
		assert.False(t, ok)
	})
}

func TestParseCSVEntry(t *testing.T) {
	t.Run("the measured record", func(t *testing.T) {
		entry, ok := parseCSVEntry([]byte(measuredBindCSV))
		require.True(t, ok)

		assert.Equal(t, "duration: 0.041 ms  execute <unnamed>: "+measuredBindStatement, entry.message)
		assert.Equal(t, "Parameters: $1 = '42', $2 = 'it''s a ''quote'''", entry.detail)
		assert.Equal(t, "3285802980598187581", entry.queryID, "the 26th column")
	})

	t.Run("a newline inside the quoted detail", func(t *testing.T) {
		entry, ok := parseCSVEntry([]byte(measuredBindCSVMultiline))
		require.True(t, ok)

		assert.Equal(t, "parameters: $1 = 'line1\nline2'", entry.detail)
		assert.Equal(t, "-6450469992402841788", entry.queryID)
	})

	t.Run("fewer columns is no identifier", func(t *testing.T) {
		entry, ok := parseCSVEntry([]byte(csvEntry("duration: 1 ms  execute <unnamed>: SELECT 1")))
		require.True(t, ok)
		assert.Empty(t, entry.queryID, "the fixture's 26th column is 0, the server's none")

		short, ok := parseCSVEntry([]byte(`a,b,c,d,e,f,g,h,i,j,k,LOG,00000,"duration: 1 ms  execute <unnamed>: SELECT 1"` + "\n"))
		require.True(t, ok)
		assert.Empty(t, short.detail)
		assert.Empty(t, short.queryID)
	})

	t.Run("not a record", func(t *testing.T) {
		_, ok := parseCSVEntry([]byte("a,b\n"))
		assert.False(t, ok)
	})
}

func TestParseJSONEntry(t *testing.T) {
	entry, ok := parseJSONEntry([]byte(measuredBindJSON))
	require.True(t, ok)

	assert.Equal(t, "duration: 0.018 ms  execute <unnamed>: "+measuredBindStatement, entry.message)
	assert.Equal(t, "Parameters: $1 = '42', $2 = 'it''s a ''quote'''", entry.detail)
	assert.Equal(t, "3285802980598187581", entry.queryID, "a JSON number, rendered back")

	multiline, ok := parseJSONEntry([]byte(measuredBindJSONMultiline))
	require.True(t, ok)
	assert.Equal(t, "Parameters: $1 = 'line1\nline2'", multiline.detail)
	assert.Equal(t, "-3411363329778510676", multiline.queryID)

	none, ok := parseJSONEntry([]byte(jsonEntryLine(t, "duration: 1 ms  execute <unnamed>: SELECT 1")))
	require.True(t, ok)
	assert.Empty(t, none.queryID, "absent is none")

	_, ok = parseJSONEntry([]byte("not json\n"))
	assert.False(t, ok)
}

// --- the record --------------------------------------------------------------

func TestExecuteRecord(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		detail  string
		ok      bool
	}{
		{name: "execute with parameters", message: "duration: 0.1 ms  execute <unnamed>: SELECT $1", detail: "Parameters: $1 = 'x'", ok: true},
		{name: "the lower-case label of 14 to 16", message: "duration: 0.1 ms  execute <unnamed>: SELECT $1", detail: "parameters: $1 = 'x'", ok: true},
		{name: "a named statement and portal", message: "duration: 0.1 ms  execute S_1/p1: SELECT $1", detail: "Parameters: $1 = 'x'", ok: true},
		{name: "a portal fetch", message: "duration: 0.1 ms  execute fetch from <unnamed>/c1: SELECT $1", detail: "Parameters: $1 = 'x'", ok: true},

		{name: "execute without parameters logged", message: "duration: 0.1 ms  execute <unnamed>: SELECT $1"},
		{name: "a bind record", message: "duration: 0.1 ms  bind <unnamed>: SELECT $1", detail: "Parameters: $1 = 'x'"},
		{name: "a parse record", message: "duration: 0.1 ms  parse <unnamed>: SELECT $1"},
		{name: "a statement record", message: "duration: 0.1 ms  statement: SELECT 1"},
		{name: "an auto_explain plan", message: "duration: 0.1 ms  plan:\nQuery Text: SELECT 1"},
		{name: "a detail that is not parameters", message: "duration: 0.1 ms  execute <unnamed>: SELECT $1", detail: "some other detail"},
		{name: "not a duration line", message: "checkpoint starting: time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record, ok := executeRecord(logEntry{message: tc.message, detail: tc.detail, queryID: "7"})

			assert.Equal(t, tc.ok, ok)

			if tc.ok {
				assert.Equal(t, "SELECT $1", record.query)
				assert.Equal(t, []bindValue{{text: "x"}}, record.values)
				assert.Equal(t, "7", record.queryID)
				assert.InDelta(t, 0.1, record.duration, 0.0001)
				assert.Empty(t, record.reason)
			}
		})
	}

	t.Run("the measured record on every format", func(t *testing.T) {
		for _, tc := range []struct {
			format logFormat
			event  string
			prefix *linePrefix
			id     string
		}{
			{logFormatStderr, measuredBindStderr, compileLinePrefix("%m [%p] "), ""},
			{logFormatStderr, measuredBindStderrPrefixed, compileLinePrefix(measuredBindPrefixTemplate), "-2760221630656037670"},
			{logFormatCSV, measuredBindCSV, nil, "3285802980598187581"},
			{logFormatJSON, measuredBindJSON, nil, "3285802980598187581"},
		} {
			entry, ok := parseLogEntry([]byte(tc.event), tc.format, tc.prefix)
			require.True(t, ok, tc.format)

			record, ok := executeRecord(entry)
			require.True(t, ok, tc.format)

			assert.Equal(t, measuredBindStatement, record.query, tc.format)
			assert.Equal(t, measuredBindValues(), record.values, tc.format)
			assert.Equal(t, tc.id, record.queryID, tc.format)
			assert.Empty(t, record.reason, tc.format)
		}
	})

	t.Run("an unusable record is still a record", func(t *testing.T) {
		record, ok := executeRecord(logEntry{
			message: "duration: 0.1 ms  execute <unnamed>: SELECT $1",
			detail:  "Parameters: $1 = 'abcdefgh...'",
			queryID: "7",
		})

		require.True(t, ok)
		assert.Equal(t, reasonBindTruncated, record.reason)
		assert.Nil(t, record.values, "no partial set")
	})
}

// --- the store ---------------------------------------------------------------

func storedRecord(id string, duration float64, value string) *bindRecord {
	return &bindRecord{queryID: id, duration: duration, query: "SELECT $1", values: []bindValue{{text: value}}}
}

func TestBindStore(t *testing.T) {
	t.Run("unretained records are counted and dropped", func(t *testing.T) {
		store := newBindStore()
		store.add(storedRecord("7", 1, "x"), false)

		assert.Equal(t, 1, store.total)
		assert.Empty(t, store.retained, "the value is not kept when the tier is off")
		assert.Empty(t, store.byID)
	})

	t.Run("no identifier is no join", func(t *testing.T) {
		store := newBindStore()
		store.add(storedRecord("", 1, "x"), true)

		assert.Equal(t, 1, store.unidentified)
		assert.Empty(t, store.retained)
	})

	t.Run("a refused record is remembered by its reason", func(t *testing.T) {
		store := newBindStore()
		store.add(&bindRecord{queryID: "7", reason: reasonBindTruncated}, true)

		assert.Equal(t, 1, store.rejected)
		assert.Equal(t, reasonBindTruncated, store.refused["7"])
		assert.Empty(t, store.retained)
	})

	t.Run("the slowest per identifier", func(t *testing.T) {
		store := newBindStore()
		store.add(storedRecord("7", 0.9, "a"), true)
		store.add(storedRecord("7", 412.5, "b"), true)
		store.add(storedRecord("7", 7.25, "c"), true)

		require.Len(t, store.retained, 1)
		assert.Equal(t, "b", store.byID["7"].values[0].text)
		assert.Equal(t, 3, store.seen["7"])
		assert.Equal(t, 2, store.dropped)
	})

	t.Run("the byte budget evicts oldest-first", func(t *testing.T) {
		store := newBindStore()
		big := strings.Repeat("x", MaxRetainedBindBytes/2+1)

		for i := range 3 {
			store.add(storedRecord(strconv.Itoa(i), 1, big), true)
		}

		require.Len(t, store.retained, 1)
		assert.Equal(t, "2", store.retained[0].queryID)
		assert.Equal(t, 2, store.dropped)
		assert.LessOrEqual(t, store.bytes, MaxRetainedBindBytes)
	})

	t.Run("attach", func(t *testing.T) {
		candidate := func(id int64) *explainCandidate {
			return &explainCandidate{queryid: ptr(id), mode: planModeNone}
		}

		store := newBindStore()
		store.add(storedRecord("7", 1, "x"), true)
		store.add(&bindRecord{queryID: "8", reason: reasonBindMalformed}, true)

		gated := candidate(7)
		store.attach([]*explainCandidate{gated}, reasonParameterCapUnbounded)
		assert.Nil(t, gated.literal)
		assert.Equal(t, reasonParameterCapUnbounded, gated.literalReason)

		first, again, refused, none := candidate(7), candidate(7), candidate(8), candidate(9)
		store.attach([]*explainCandidate{first, again, refused, none}, "")

		require.NotNil(t, first.literal)
		assert.Equal(t, "x", first.literal.values[0].text)
		assert.Empty(t, first.literalReason)
		assert.Empty(t, store.retained, "claimed, so no longer held against the budget")

		assert.Nil(t, again.literal)
		assert.Equal(t, reasonBindClaimed, again.literalReason)

		assert.Equal(t, reasonBindMalformed, refused.literalReason, "the refusal it met, not a bare absence")
		assert.Equal(t, reasonNoBindRecord, none.literalReason)

		logged := candidate(7)
		logged.mode = planModeLogged
		store.attach([]*explainCandidate{logged}, "")
		assert.Empty(t, logged.literalReason, "a candidate the LOGGED tier took is left alone")
	})
}
