package postgres

import (
	"bytes"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// logRedaction is what a log tail replaces in each event before writing it: the SQL
// text and the values the event carries, never its names, codes or numbers. Each
// replacement is one redactedValue, and the block's redacted= counts them. An event is
// otherwise written as read: in csvlog and jsonlog only the fields that changed are
// re-encoded, the way the server encodes them.
type logRedaction struct {
	// deadlockReport reads DETAIL as a deadlock report: its lock lines are kept, the
	// text after each "Process <n>: " is replaced, and so is any line of another shape.
	// csvlog and jsonlog take a translated report by its code, where no English shape
	// would find the SQL in it.
	deadlockReport bool

	// plans reads the message as an auto_explain entry: its first line, then a plan,
	// whose constants the plan rules replace.
	plans bool
}

// logField is the part of an entry a text belongs to, which decides what is replaced.
type logField int

const (
	fieldMessage logField = iota
	fieldDetail
	fieldHint
	fieldInternalQuery
	fieldContext
	fieldStatement
	fieldLocation
)

// field redacts one field's text. Lines within it are separated by "\n", whatever the
// format writes between them. sqlstate is the entry's code where the format carries
// one, and empty where it does not.
func (r *logRedaction) field(sqlstate string, kind logField, text string) (string, int) {
	if text == "" {
		return text, 0
	}

	switch kind {
	case fieldStatement, fieldInternalQuery:
		return redactedValue, 1

	case fieldMessage:
		if r.plans {
			return redactPlanEntry(text)
		}

		return redactMessage(text)

	case fieldContext:
		return redactContext(text)

	case fieldDetail:
		if r.deadlockReport {
			return redactDeadlockReport(text)
		}

		return redactDetail(sqlstate, text)
	}

	return text, 0
}

// event redacts one copied event in the format it was read in.
func (r *logRedaction) event(event []byte, format logFormat) ([]byte, int) {
	switch format {
	case logFormatCSV:
		return r.csvRecord(event)

	case logFormatJSON:
		return r.jsonRecord(event)
	}

	return r.stderrEvent(event)
}

// redactedEvent stands in for an event whose fields could not be told apart, so that
// nothing in it is written.
func redactedEvent() ([]byte, int) { return []byte(redactedValue + "\n"), 1 }

// redactShapes replaces the values in the first shape s is in: every group of a shape
// is a value.
func redactShapes(shapes []*regexp.Regexp, s string) (string, int) {
	for _, shape := range shapes {
		at := shape.FindStringSubmatchIndex(s)
		if at == nil {
			continue
		}

		var out strings.Builder

		copied, redacted := 0, 0

		for group := 2; group < len(at); group += 2 {
			if at[group] < 0 {
				continue
			}

			out.WriteString(s[copied:at[group]])
			out.WriteString(redactedValue)

			copied = at[group+1]
			redacted++
		}

		out.WriteString(s[copied:])

		return out.String(), redacted
	}

	return s, 0
}

// messageValues are the error messages that quote a value from the statement or the
// data rather than a name. PostgreSQL quotes names and values alike, so only a known
// shape tells them apart: relation "orders" stays. English only, as every tail's
// matching is.
var messageValues = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^invalid input syntax for (?:type )?[^:"]+: "(.*)"$`),
	regexp.MustCompile(`(?s)^invalid input value for enum [^:"]+: "(.*)"$`),
	regexp.MustCompile(`(?s)^value "(.*)" is out of range for type [^"]+$`),
	regexp.MustCompile(`(?s)^"(.*)" is out of range for type [^"]+$`),
	regexp.MustCompile(`(?s)^(?:date/time|interval) field value out of range: "(.*)"$`),
	regexp.MustCompile(`(?s)^invalid value "(.*)" for "[^"]*"$`),
	regexp.MustCompile(`(?s)^malformed (?:array|range|multirange|record) literal: "(.*)"$`),
	regexp.MustCompile(`(?s)^invalid byte sequence for encoding "[^"]+": (.+)$`),
	regexp.MustCompile(`(?s)^character with byte sequence (.+) in encoding "[^"]+" has no equivalent in encoding "[^"]+"$`),
	regexp.MustCompile(`(?s)^unterminated (?:quoted string|dollar-quoted string|bit string literal|hexadecimal string literal) at or near "(.*)"$`),
	regexp.MustCompile(`(?s)^trailing junk after (?:numeric literal|parameter) at or near "(.*)"$`),
	regexp.MustCompile(`(?s)^syntax error at or near "((?:[EeBbXxNn]|[Uu]&)?'.*|\$.*|[0-9.].*)"$`),
	regexp.MustCompile(`(?s)^invalid value for parameter "[^"]+": "(.*)"$`),
	regexp.MustCompile(`(?s)^string is not a valid identifier: "(.*)"$`),
	regexp.MustCompile(`(?s)^invalid hexadecimal digit: "(.*)"$`),
	regexp.MustCompile(`(?s)^time zone "(.*)" not recognized$`),
}

// verbosePrefix is the code log_error_verbosity=verbose writes before a stderr message,
// and cursorSuffix where in the statement the error was, which stderr writes after it.
var (
	verbosePrefix = regexp.MustCompile(`^[0-9A-Z]{5}: `)
	cursorSuffix  = regexp.MustCompile(` at character \d+$`)
)

// pgErrorText is a server error as the driver renders it: level, message and code.
var pgErrorText = regexp.MustCompile(`^([A-Z]+: )(.*?)( \(SQLSTATE [0-9A-Z]{5}\))?$`)

// redactErrorText is redactMessage for an error the agent's own statement got, as an
// error= header carries it.
func redactErrorText(text string) (string, int) {
	match := pgErrorText.FindStringSubmatch(text)
	if match == nil {
		return text, 0
	}

	message, n := redactMessage(match[2])
	if n == 0 {
		return text, 0
	}

	return match[1] + message + match[3], n
}

func redactMessage(text string) (string, int) {
	code := verbosePrefix.FindString(text)
	body := text[len(code):]
	cursor := cursorSuffix.FindString(body)

	redacted, n := redactShapes(messageValues, body[:len(body)-len(cursor)])
	if n == 0 {
		return text, 0
	}

	return code + redacted + cursor, n
}

// detailValues are the DETAIL texts that quote a key's or a row's values, or the input
// the json parser stopped at.
var detailValues = []*regexp.Regexp{
	regexp.MustCompile(`(?s)^Key \(.*?\)=\((.*)\) (?:already exists|is duplicated)\.$`),
	regexp.MustCompile(`(?s)^Key \(.*?\)=\((.*)\) is (?:not present in|still referenced from) table ".*"\.$`),
	regexp.MustCompile(`(?s)^Key \(.*?\)=\((.*)\) conflicts with (?:existing )?key \(.*?\)=\((.*)\)\.$`),
	regexp.MustCompile(`(?s)^Failing row contains \((.*)\)\.$`),
	regexp.MustCompile(`(?s)^Partition key of the failing row contains \(.*?\) = \((.*)\)\.$`),
	regexp.MustCompile(`(?s)^Token "(.*)" is invalid\.$`),
	regexp.MustCompile(`(?s)^Expected .*, but found "(.*)"\.$`),
	regexp.MustCompile(`(?s)^Escape sequence "(.*)" is invalid\.$`),
}

func redactDetail(sqlstate, text string) (string, int) {
	if redacted, n := redactShapes(detailValues, text); n > 0 {
		return redacted, n
	}

	// A key in a shape not listed, a translated one among them: its values run from
	// the first ")=(" to the last ")".
	if at := strings.Index(text, ")=("); at >= 0 {
		if end := strings.LastIndexByte(text, ')'); end >= at+3 {
			return text[:at+3] + redactedValue + text[end:], 1
		}
	}

	// An integrity violation's DETAIL names the key or row that broke it, in whatever
	// language the server writes, so one in no shape above is replaced whole.
	if strings.HasPrefix(sqlstate, "23") || sqlstate == "44000" {
		return redactedValue, 1
	}

	return text, 0
}

// A deadlock report's DETAIL names each process's lock wait, then each process's
// statement, which may run over several lines.
var (
	deadlockWait  = regexp.MustCompile(`^Process \d+ waits for .+; blocked by process \d+\.$`)
	deadlockQuery = regexp.MustCompile(`^Process \d+: `)
)

func redactDeadlockReport(text string) (string, int) {
	var (
		out       []string
		redacted  int
		runningOn bool
	)

	for line := range strings.SplitSeq(text, "\n") {
		if deadlockWait.MatchString(line) {
			out = append(out, line)
			runningOn = false

			continue
		}

		if at := deadlockQuery.FindStringIndex(line); at != nil {
			out = append(out, line[:at[1]]+redactedValue)
			redacted++
			runningOn = true

			continue
		}

		// The line before was replaced, and its statement runs on into this one.
		if runningOn {
			continue
		}

		out = append(out, redactedValue)
		redacted++
		runningOn = true
	}

	if redacted == 0 {
		return text, 0
	}

	return strings.Join(out, "\n"), redacted
}

// CONTEXT holds one frame per line, innermost first. Frames that quote SQL text or
// carry row data or parameter values have that part replaced; the rest name
// functions, relations and positions and are kept.
var (
	// contextValues are the frames whose text after the match is replaced, and what
	// closes that text: SPI's for the SQL a function ran or an expression PL/pgSQL
	// evaluated, COPY's for a column's value or a whole line of input, and the json
	// parser's for the input around the error.
	contextValues = []struct {
		open  *regexp.Regexp
		close string
	}{
		{regexp.MustCompile(`^(?:SQL statement|SQL expression|PL/pgSQL assignment) "`), `"`},
		{regexp.MustCompile(`^COPY .*?, line \d+(?:, column .*?)?: "`), `"`},
		{regexp.MustCompile(`^JSON data, line \d+: `), ""},
	}

	// contextParameters is the extended protocol's frame for a portal's bind values.
	contextParameters = regexp.MustCompile(`^(?:unnamed portal|portal ".*?") (?:with parameters: |parameter )`)

	// contextFrame opens a frame, which ends a replaced one's text running on.
	contextFrame = regexp.MustCompile(`^(?:PL/pgSQL function |SQL function "|SQL statement "|SQL expression "|` +
		`PL/pgSQL assignment "|COPY |JSON data, line |unnamed portal |portal "|while |automatic (?:vacuum|analyze) |` +
		`parallel worker|writing block |processing remote data )`)
)

func redactContext(text string) (string, int) {
	var (
		out       []string
		redacted  int
		runningOn bool
	)

	for line := range strings.SplitSeq(text, "\n") {
		// A replaced frame's text can hold newlines; it runs until a line opens a frame.
		if runningOn && !contextFrame.MatchString(line) {
			continue
		}

		runningOn = false

		for _, frame := range contextValues {
			if at := frame.open.FindStringIndex(line); at != nil {
				line = line[:at[1]] + redactedValue + frame.close
				redacted++
				runningOn = true

				break
			}
		}

		if at := contextParameters.FindStringIndex(line); at != nil && !runningOn {
			values, n := redactParameterValues(line[at[1]:])
			line = line[:at[1]] + values
			redacted += n
			runningOn = n > 0
		}

		out = append(out, line)
	}

	if redacted == 0 {
		return text, 0
	}

	return strings.Join(out, "\n"), redacted
}

// parameterName is one "$<n> = " of a parameter list.
var parameterName = regexp.MustCompile(`\$\d+ = `)

// redactParameterValues replaces each quoted value in "$1 = '…', $2 = NULL": the
// server doubles a quote inside a value, so a value holding ", $2 = " is still one
// value. NULL is not a value and stays. A quote left open runs to the end.
func redactParameterValues(s string) (string, int) {
	var out strings.Builder

	redacted := 0

	for {
		at := parameterName.FindStringIndex(s)
		if at == nil {
			out.WriteString(s)

			return out.String(), redacted
		}

		out.WriteString(s[:at[1]])
		s = s[at[1]:]

		if !strings.HasPrefix(s, "'") {
			continue
		}

		out.WriteString("'" + redactedValue + "'")
		redacted++

		s = s[quotedLength(s):]
	}
}

// quotedLength is the length of the SQL string literal s opens with, both quotes
// included and a doubled quote read as one inside it; an unterminated one runs to
// the end.
func quotedLength(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] != '\'' {
			continue
		}

		if i+1 < len(s) && s[i+1] == '\'' {
			i++

			continue
		}

		return i + 1
	}

	return len(s)
}

// stderrFieldKinds maps each secondary keyword to its field.
var stderrFieldKinds = map[string]logField{
	"DETAIL":    fieldDetail,
	"HINT":      fieldHint,
	"QUERY":     fieldInternalQuery,
	"CONTEXT":   fieldContext,
	"STATEMENT": fieldStatement,
	"LOCATION":  fieldLocation,
}

// stderrField is one field of a stderr entry as read: head is its first line up to the
// text, raw its lines with their terminators, text their text without the TAB the
// server puts before each line after the first.
type stderrField struct {
	kind logField
	head string
	raw  []string
	text []string
}

// stderrEvent splits the entry into its fields the way the matcher bounds it: a TAB
// line continues the field before it, and a line opening a secondary keyword starts
// the next. A field that changed is written again, its lines joined by a newline and a
// TAB as the server writes them; one that did not is copied.
func (r *logRedaction) stderrEvent(event []byte) ([]byte, int) {
	lines := strings.SplitAfter(string(event), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) == 0 {
		return event, 0
	}

	first, _ := cutTerminator(lines[0])

	at, _, message := stderrSeverity(first)
	if at < 0 {
		return redactedEvent()
	}

	prefix := first[:at]
	sqlstate := strings.TrimSuffix(verbosePrefix.FindString(message), ": ")

	var (
		out      bytes.Buffer
		redacted int
	)

	field := stderrField{kind: fieldMessage, head: first[:len(first)-len(message)]}
	field.raw, field.text = []string{lines[0]}, []string{message}

	for _, line := range lines[1:] {
		body, _ := cutTerminator(line)

		if kind, head, text, ok := stderrFieldLine(body, prefix); ok {
			redacted += r.writeStderrField(&out, sqlstate, field)

			field = stderrField{kind: kind, head: head}
			field.raw, field.text = []string{line}, []string{text}

			continue
		}

		field.raw = append(field.raw, line)
		field.text = append(field.text, strings.TrimPrefix(body, "\t"))
	}

	redacted += r.writeStderrField(&out, sqlstate, field)

	if redacted == 0 {
		return event, 0
	}

	return out.Bytes(), redacted
}

func (r *logRedaction) writeStderrField(out *bytes.Buffer, sqlstate string, f stderrField) int {
	text, redacted := r.field(sqlstate, f.kind, strings.Join(f.text, "\n"))
	if redacted == 0 {
		for _, line := range f.raw {
			out.WriteString(line)
		}

		return 0
	}

	_, eol := cutTerminator(f.raw[0])
	if eol == "" {
		eol = "\n"
	}

	_, last := cutTerminator(f.raw[len(f.raw)-1])

	out.WriteString(f.head)
	out.WriteString(strings.ReplaceAll(text, "\n", eol+"\t"))
	out.WriteString(last)

	return redacted
}

// stderrFieldLine reports whether a line opens a secondary field, as isNewStderrEntry
// reads one: at the entry's own prefix, or, on a line without it, at the earliest
// keyword, since %l in log_line_prefix numbers each line of an entry apart.
func stderrFieldLine(body, prefix string) (kind logField, head, text string, ok bool) {
	if strings.HasPrefix(body, "\t") {
		return 0, "", "", false
	}

	at := len(prefix)

	if !strings.HasPrefix(body, prefix) {
		at = keywordIndex(body, secondaryKeywords)
		if at < 0 {
			return 0, "", "", false
		}
	}

	for _, keyword := range secondaryKeywords {
		if rest, found := keywordAt(body[at:], []string{keyword}); found {
			return stderrFieldKinds[keyword], body[:len(body)-len(rest)], rest, true
		}
	}

	return 0, "", "", false
}

// cutTerminator splits a line from its "\n" or "\r\n".
func cutTerminator(line string) (body, eol string) {
	if body, found := strings.CutSuffix(line, "\r\n"); found {
		return body, "\r\n"
	}

	if body, found := strings.CutSuffix(line, "\n"); found {
		return body, "\n"
	}

	return line, ""
}

// csvFieldKinds are the columns that hold text an entry can carry values in.
var csvFieldKinds = map[int]logField{
	csvMessageIndex:       fieldMessage,
	csvDetailIndex:        fieldDetail,
	csvHintIndex:          fieldHint,
	csvInternalQueryIndex: fieldInternalQuery,
	csvContextIndex:       fieldContext,
	csvStatementIndex:     fieldStatement,
}

// csvRecord replaces text inside the record's fields. A field that changed is quoted
// again as csvlog quotes one; the other bytes are copied.
func (r *logRedaction) csvRecord(record []byte) ([]byte, int) {
	spans, ok := csvFieldSpans(record)
	if !ok || len(spans) <= csvMessageIndex {
		return redactedEvent()
	}

	var (
		out      bytes.Buffer
		redacted int
		copied   int
	)

	sqlstate := csvValue(record, spans[csvStateIndex])

	for i, span := range spans {
		kind, carries := csvFieldKinds[i]
		if !carries {
			continue
		}

		// Unquoted is empty: csvlog quotes every text it writes.
		if record[span[0]] != '"' {
			continue
		}

		text, n := r.field(sqlstate, kind, csvValue(record, span))
		if n == 0 {
			continue
		}

		out.Write(record[copied:span[0]])
		out.WriteString(`"` + strings.ReplaceAll(text, `"`, `""`) + `"`)

		copied = span[1]
		redacted += n
	}

	if redacted == 0 {
		return record, 0
	}

	out.Write(record[copied:])

	return out.Bytes(), redacted
}

// csvValue is a field's text, its quotes removed and a doubled quote read as one.
func csvValue(record []byte, span [2]int) string {
	raw := string(record[span[0]:span[1]])
	if !strings.HasPrefix(raw, `"`) {
		return raw
	}

	return strings.ReplaceAll(raw[1:len(raw)-1], `""`, `"`)
}

// csvFieldSpans splits one csvlog record into the byte spans of its fields, quotes
// included; the line terminator belongs to none. False when the record is not
// csvlog's shape: a quote left open, or bytes after a closing quote.
func csvFieldSpans(record []byte) ([][2]int, bool) {
	var spans [][2]int

	pos := 0

	for {
		start := pos

		if pos < len(record) && record[pos] == '"' {
			pos++

			for {
				at := bytes.IndexByte(record[pos:], '"')
				if at < 0 {
					return nil, false
				}

				pos += at + 1

				if pos < len(record) && record[pos] == '"' {
					pos++

					continue
				}

				break
			}
		} else {
			for pos < len(record) && record[pos] != ',' && record[pos] != '\n' && record[pos] != '\r' {
				pos++
			}
		}

		spans = append(spans, [2]int{start, pos})

		if pos < len(record) && record[pos] == ',' {
			pos++

			continue
		}

		rest := string(record[pos:])
		if rest != "" && rest != "\n" && rest != "\r\n" {
			return nil, false
		}

		return spans, true
	}
}

// jsonFieldKinds are the keys that hold text an entry can carry values in.
var jsonFieldKinds = map[string]logField{
	"message":        fieldMessage,
	"detail":         fieldDetail,
	"hint":           fieldHint,
	"internal_query": fieldInternalQuery,
	"context":        fieldContext,
	"statement":      fieldStatement,
}

// jsonRecord replaces text inside the line's values. A value that changed is escaped
// again as jsonlog escapes one; the other bytes are copied.
func (r *logRedaction) jsonRecord(line []byte) ([]byte, int) {
	values, ok := jsonValueSpans(line)
	if !ok {
		return redactedEvent()
	}

	var (
		out      bytes.Buffer
		redacted int
		copied   int
	)

	// Absent is 00000, as the matcher reads it.
	sqlstate := "00000"

	for _, v := range values {
		if v.key == "state_code" && line[v.start] == '"' {
			sqlstate = unescapeJSON(line[v.start+1 : v.end-1])
		}
	}

	for _, v := range values {
		kind, carries := jsonFieldKinds[v.key]
		if !carries || line[v.start] != '"' {
			continue
		}

		text, n := r.field(sqlstate, kind, unescapeJSON(line[v.start+1:v.end-1]))
		if n == 0 {
			continue
		}

		out.Write(line[copied:v.start])
		out.WriteString(escapeJSON(text))

		copied = v.end
		redacted += n
	}

	if redacted == 0 {
		return line, 0
	}

	out.Write(line[copied:])

	return out.Bytes(), redacted
}

// jsonValue is where one key's value sits in a line.
type jsonValue struct {
	key        string
	start, end int
}

// jsonValueSpans finds each top-level key's value in one JSON object. False when the
// line is not one object.
func jsonValueSpans(line []byte) ([]jsonValue, bool) {
	var values []jsonValue

	pos := skipJSONSpace(line, 0)
	if pos >= len(line) || line[pos] != '{' {
		return nil, false
	}

	pos = skipJSONSpace(line, pos+1)

	if pos < len(line) && line[pos] == '}' {
		return values, true
	}

	for {
		if pos >= len(line) || line[pos] != '"' {
			return nil, false
		}

		keyEnd, ok := jsonStringEnd(line, pos)
		if !ok {
			return nil, false
		}

		key := unescapeJSON(line[pos+1 : keyEnd-1])

		pos = skipJSONSpace(line, keyEnd)
		if pos >= len(line) || line[pos] != ':' {
			return nil, false
		}

		start := skipJSONSpace(line, pos+1)

		end, ok := jsonValueEnd(line, start)
		if !ok {
			return nil, false
		}

		values = append(values, jsonValue{key: key, start: start, end: end})

		pos = skipJSONSpace(line, end)
		if pos >= len(line) {
			return nil, false
		}

		switch line[pos] {
		case ',':
			pos = skipJSONSpace(line, pos+1)

		case '}':
			return values, true

		default:
			return nil, false
		}
	}
}

func skipJSONSpace(line []byte, pos int) int {
	for pos < len(line) && strings.IndexByte(" \t\r\n", line[pos]) >= 0 {
		pos++
	}

	return pos
}

// jsonStringEnd is the offset just past the string that opens at pos.
func jsonStringEnd(line []byte, pos int) (int, bool) {
	for i := pos + 1; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++

		case '"':
			return i + 1, true
		}
	}

	return 0, false
}

// jsonValueEnd is the offset just past the value that opens at pos: a string, an object
// or array (its strings skipped whole), or a number or literal.
func jsonValueEnd(line []byte, pos int) (int, bool) {
	if pos >= len(line) {
		return 0, false
	}

	switch line[pos] {
	case '"':
		return jsonStringEnd(line, pos)

	case '{', '[':
		depth := 0

		for i := pos; i < len(line); i++ {
			switch line[i] {
			case '"':
				end, ok := jsonStringEnd(line, i)
				if !ok {
					return 0, false
				}

				i = end - 1

			case '{', '[':
				depth++

			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1, true
				}
			}
		}

		return 0, false
	}

	end := pos
	for end < len(line) && strings.IndexByte(",}] \t\r\n", line[end]) < 0 {
		end++
	}

	return end, end > pos
}

// unescapeJSON reads a JSON string's content. Bytes that are not an escape are kept as
// they are, so a log in a non-UTF-8 encoding keeps its bytes.
func unescapeJSON(s []byte) string {
	var out strings.Builder

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out.WriteByte(s[i])

			continue
		}

		i++

		switch s[i] {
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'u':
			r, n := unescapeJSONRune(s[i+1:])
			out.WriteRune(r)
			i += n
		default:
			out.WriteByte(s[i])
		}
	}

	return out.String()
}

// unescapeJSONRune reads the four hex digits after \u, and a second \u escape where the
// first is half of a surrogate pair. It returns how many bytes it read.
func unescapeJSONRune(s []byte) (rune, int) {
	if len(s) < 4 {
		return utf8.RuneError, 0
	}

	v, err := strconv.ParseUint(string(s[:4]), 16, 32)
	if err != nil {
		return utf8.RuneError, 0
	}

	r := rune(v)

	if r >= 0xd800 && r < 0xdc00 && len(s) >= 10 && s[4] == '\\' && s[5] == 'u' {
		if low, err := strconv.ParseUint(string(s[6:10]), 16, 32); err == nil && low >= 0xdc00 && low < 0xe000 {
			return (r-0xd800)<<10 + (rune(low) - 0xdc00) + 0x10000, 10
		}
	}

	return r, 4
}

// escapeJSON quotes s as jsonlog does: the two-character escapes, \u00xx for the other
// control characters, every other byte as it is.
func escapeJSON(s string) string {
	var out strings.Builder

	out.WriteByte('"')

	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		default:
			if c < ' ' {
				out.WriteString(`\u00`)
				out.WriteString(strconv.FormatUint(uint64(c)>>4, 16))
				out.WriteString(strconv.FormatUint(uint64(c)&0xf, 16))

				continue
			}

			out.WriteByte(c)
		}
	}

	out.WriteByte('"')

	return out.String()
}
