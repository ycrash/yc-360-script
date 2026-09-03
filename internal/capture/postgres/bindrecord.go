package postgres

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// The literal tier's evidence is the server's own log. Under
// log_min_duration_statement, an extended-protocol execution is logged as
// "duration: <n> ms  execute <name>: <statement>" with a DETAIL of
// "Parameters: $1 = 'value', $2 = NULL" whenever log_parameter_max_length is not zero.
// The statement text in that message is what the values are positional for - the
// normalized text in pg_stat_statements numbers its placeholders past the constants
// it replaced, so it is not - and it is the text the literal tier prepares.
//
// Measured on the matrix, 2026-09-03: the detail reads "parameters:" on 14 through 16
// and "Parameters:" on 17 and 18; a value is single-quoted with quotes doubled; NULL is
// the bare word; a value the cap clipped ends "..." inside its quotes, cut on a
// character boundary at or under the cap, and a genuine value ending "..." is
// indistinguishable from one, so both are refused. csvlog carries the identifier in
// its 26th column and jsonlog in query_id; stderr carries it only where
// log_line_prefix has %Q. The join is that identifier and nothing else.

// bindValue is one decoded parameter: SQL NULL, or text as the server printed it.
type bindValue struct {
	null bool
	text string
}

// bindRecord is one execute record with its parameters, kept for the literal tier.
type bindRecord struct {
	queryID  string
	duration float64

	// query is the statement text the record itself carries.
	query  string
	values []bindValue

	// reason is why the record is unusable; empty means the parser accepted it.
	reason string
}

// size is what the record costs the retention budget.
func (r *bindRecord) size() int {
	n := len(r.query)

	for _, v := range r.values {
		n += len(v.text)
	}

	return n
}

// MaxRetainedBindBytes bounds retained bind records across the window, oldest-first,
// as MaxRetainedPlanBytes bounds plans.
const MaxRetainedBindBytes = 1 << 20

// parameterClipMark is what the server appends, inside the quotes, to a value it cut
// at log_parameter_max_length.
const parameterClipMark = "..."

// logEntry is what the three formats have in common once parsed: the message, the
// detail and the identifier the format carries. An empty queryID is "none proven".
type logEntry struct {
	message string
	detail  string
	queryID string
}

// parseLogEntry reads one matched event back into its fields.
func parseLogEntry(event []byte, format logFormat, prefix *linePrefix) (logEntry, bool) {
	switch format {
	case logFormatCSV:
		return parseCSVEntry(event)

	case logFormatJSON:
		return parseJSONEntry(event)
	}

	return parseStderrEntry(event, prefix)
}

// parseStderrEntry reassembles a multi-line stderr event: the message is the first
// line's text after the severity keyword plus its TAB-continued lines, and the detail
// is the DETAIL: line's text plus its own. The server writes one TAB after every
// embedded newline (append_with_tabs), so removing exactly one restores the text.
func parseStderrEntry(event []byte, prefix *linePrefix) (logEntry, bool) {
	lines := strings.Split(strings.TrimSuffix(string(event), "\n"), "\n")

	at, message := stderrSeverity(lines[0])
	if at < 0 {
		return logEntry{}, false
	}

	entryPrefix := lines[0][:at]
	entry := logEntry{message: message, queryID: prefix.queryID(entryPrefix)}

	current := &entry.message

	for _, line := range lines[1:] {
		switch {
		case strings.HasPrefix(line, "\t"):
			if current != nil {
				*current += "\n" + line[1:]
			}

		case strings.HasPrefix(line, entryPrefix):
			rest, ok := strings.CutPrefix(line[len(entryPrefix):], "DETAIL:")
			if !ok {
				current = nil

				continue
			}

			entry.detail = strings.TrimLeft(rest, " \t")
			current = &entry.detail

		default:
			current = nil
		}
	}

	return entry, true
}

func parseCSVEntry(event []byte) (logEntry, bool) {
	reader := csv.NewReader(bytes.NewReader(event))
	reader.FieldsPerRecord = -1

	record, err := reader.Read()
	if err != nil || len(record) <= csvMessageIndex {
		return logEntry{}, false
	}

	entry := logEntry{message: record[csvMessageIndex]}

	if len(record) > csvDetailIndex {
		entry.detail = record[csvDetailIndex]
	}

	if len(record) > csvQueryIDIndex {
		entry.queryID = identifierText(record[csvQueryIDIndex])
	}

	return entry, true
}

func parseJSONEntry(event []byte) (logEntry, bool) {
	var parsed jsonEntry
	if err := json.Unmarshal(bytes.TrimSpace(event), &parsed); err != nil {
		return logEntry{}, false
	}

	entry := logEntry{message: parsed.Message, detail: parsed.Detail}

	if parsed.QueryID != 0 {
		entry.queryID = strconv.FormatInt(parsed.QueryID, 10)
	}

	return entry, true
}

// identifierText is a query identifier as the log printed it, or empty: zero is what
// the server writes when none was computed, and anything but an integer is no proof.
func identifierText(s string) string {
	if s == "" || s == "0" {
		return ""
	}

	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return ""
	}

	return s
}

// executeRecord reads a log_min_duration_statement execute record out of an entry.
// False for everything else the matcher passed - an auto_explain plan, or an execute
// record with no parameters logged, which is a slow-statement line and not bind
// evidence. The parse or bind records of the same execution are not taken: they carry
// the same values under the phase's own duration, and one execution is one record.
func executeRecord(entry logEntry) (*bindRecord, bool) {
	rest, ok := strings.CutPrefix(entry.message, "duration: ")
	if !ok {
		return nil, false
	}

	_, rest, ok = strings.Cut(rest, " ms  ")
	if !ok {
		return nil, false
	}

	rest, ok = cutExecuteVerb(rest)
	if !ok {
		return nil, false
	}

	// "<name>: <text>" or "<name>/<portal>: <text>". A name containing ": " would cut
	// short and leave a text the server refuses at PREPARE, which the block records.
	_, query, ok := strings.Cut(rest, ": ")
	if !ok {
		return nil, false
	}

	parameters, ok := cutParameters(entry.detail)
	if !ok {
		return nil, false
	}

	record := &bindRecord{queryID: entry.queryID, query: query}

	if match := planDuration.FindStringSubmatch(entry.message); match != nil {
		record.duration, _ = strconv.ParseFloat(match[1], 64)
	}

	record.values, record.reason = parseBindParameters(parameters)

	return record, true
}

// cutExecuteVerb strips "execute " or "execute fetch from ", the two spellings of an
// Execute message's log line.
func cutExecuteVerb(s string) (string, bool) {
	if rest, ok := strings.CutPrefix(s, "execute fetch from "); ok {
		return rest, true
	}

	return strings.CutPrefix(s, "execute ")
}

// cutParameters strips the detail's "Parameters: " label in either of the server's
// spellings.
func cutParameters(detail string) (string, bool) {
	const label = "parameters: "

	if len(detail) < len(label) || !strings.EqualFold(detail[:len(label)], label) {
		return "", false
	}

	return detail[len(label):], true
}

// parseBindParameters decodes "$1 = 'a', $2 = NULL" into positional values. Positions
// may arrive in any order and are restored; a missing or duplicate position, a value
// that is neither NULL nor quoted, an unterminated quote or anything between entries
// is malformed; a value ending in the clip marker is truncated. Either refusal leaves
// the candidate to the generic tier: a partial parameter set is not a literal plan.
func parseBindParameters(s string) ([]bindValue, string) {
	byPosition := map[int]bindValue{}
	truncated := false

	rest := s

	for {
		position, after, ok := cutPosition(rest)
		if !ok {
			return nil, reasonBindMalformed
		}

		if _, duplicate := byPosition[position]; duplicate {
			return nil, reasonBindMalformed
		}

		value, after, ok := cutValue(after)
		if !ok {
			return nil, reasonBindMalformed
		}

		if !value.null && strings.HasSuffix(value.text, parameterClipMark) {
			truncated = true
		}

		byPosition[position] = value

		if after == "" {
			break
		}

		rest, ok = strings.CutPrefix(after, ", ")
		if !ok {
			return nil, reasonBindMalformed
		}
	}

	values := make([]bindValue, len(byPosition))

	for i := range values {
		value, ok := byPosition[i+1]
		if !ok {
			return nil, reasonBindMalformed
		}

		values[i] = value
	}

	if truncated {
		return nil, reasonBindTruncated
	}

	return values, ""
}

// cutPosition reads "$<n> = " and returns n.
func cutPosition(s string) (position int, rest string, ok bool) {
	digits, ok := strings.CutPrefix(s, "$")
	if !ok {
		return 0, "", false
	}

	end := 0
	for end < len(digits) && digits[end] >= '0' && digits[end] <= '9' {
		end++
	}

	position, err := strconv.Atoi(digits[:end])
	if end == 0 || err != nil || position < 1 {
		return 0, "", false
	}

	rest, ok = strings.CutPrefix(digits[end:], " = ")

	return position, rest, ok
}

// cutValue reads the bare word NULL or a quoted string, un-doubling the quote marks
// on the way. The quoted string 'NULL' is text, as it is to the server.
func cutValue(s string) (value bindValue, rest string, ok bool) {
	if after, isNull := strings.CutPrefix(s, "NULL"); isNull {
		return bindValue{null: true}, after, true
	}

	quoted, ok := strings.CutPrefix(s, "'")
	if !ok {
		return bindValue{}, "", false
	}

	var text strings.Builder

	for i := 0; i < len(quoted); i++ {
		if quoted[i] != '\'' {
			text.WriteByte(quoted[i])

			continue
		}

		if i+1 < len(quoted) && quoted[i+1] == '\'' {
			text.WriteByte('\'')
			i++

			continue
		}

		return bindValue{text: text.String()}, quoted[i+1:], true
	}

	return bindValue{}, "", false
}

// renderArgument writes one decoded value as the SQL the EXECUTE carries: the bare
// word for NULL, else an escape-string literal with quotes and backslashes doubled. An
// escape string is read the same way under either standard_conforming_strings, so the
// value the server logged is the value it plans with.
func renderArgument(v bindValue) string {
	if v.null {
		return "NULL"
	}

	escaped := strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(v.text)

	return "E'" + escaped + "'"
}

// --- the stderr prefix -------------------------------------------------------

// linePrefix is log_line_prefix compiled to a matcher for the expanded prefix, with %Q
// as its one capture. Nil, or a template without %Q, identifies nothing: a stderr
// entry then has no proven identifier, and matching by query text is not one.
type linePrefix struct {
	template string
	re       *regexp.Regexp
}

// compileLinePrefix translates the template: literals are matched as themselves, %Q
// captures an integer, %q contributes nothing (a session backend's line carries the
// whole prefix), and every other escape - free text, timestamps, identifiers - matches
// lazily up to the next literal. A field-width prefix such as %-10a is skipped.
func compileLinePrefix(template string) *linePrefix {
	var (
		pattern  strings.Builder
		captures int
	)

	pattern.WriteString("^")

	for i := 0; i < len(template); i++ {
		if template[i] != '%' {
			pattern.WriteString(regexp.QuoteMeta(template[i : i+1]))

			continue
		}

		i++
		for i < len(template) && (template[i] == '-' || (template[i] >= '0' && template[i] <= '9')) {
			i++
		}

		if i >= len(template) {
			pattern.WriteString("%")

			break
		}

		switch template[i] {
		case '%':
			pattern.WriteString("%")

		case 'Q':
			pattern.WriteString(`\s*(-?[0-9]+)\s*`)
			captures++

		case 'q':

		default:
			pattern.WriteString(".*?")
		}
	}

	pattern.WriteString("$")

	prefix := &linePrefix{template: template}

	if captures > 0 {
		prefix.re = regexp.MustCompile(pattern.String())
	}

	return prefix
}

// queryID reads %Q out of an expanded prefix, or returns empty.
func (p *linePrefix) queryID(prefix string) string {
	if p == nil || p.re == nil {
		return ""
	}

	match := p.re.FindStringSubmatch(prefix)
	if match == nil {
		return ""
	}

	return identifierText(match[1])
}

// --- the store ---------------------------------------------------------------

// bindStore keeps the slowest usable record per query identifier, under
// MaxRetainedBindBytes with oldest-first eviction. What it refuses it counts, and it
// remembers why per identifier, so a candidate can say which refusal it met.
type bindStore struct {
	retained []*bindRecord
	byID     map[string]*bindRecord

	// seen counts usable identified records per identifier; survives eviction.
	seen map[string]int

	// refused is the latest parser refusal per identifier.
	refused map[string]string

	// claimed marks identifiers a candidate took.
	claimed map[string]bool

	total        int
	unidentified int
	rejected     int
	dropped      int

	bytes int
}

func newBindStore() *bindStore {
	return &bindStore{
		byID:    map[string]*bindRecord{},
		seen:    map[string]int{},
		refused: map[string]string{},
		claimed: map[string]bool{},
	}
}

// add counts the record and retains it only when retain is true - the literal tier
// is on and the source-side cap is finite - and it carries an identifier and parsed.
// An unretained record's values are dropped here and nowhere else.
func (b *bindStore) add(record *bindRecord, retain bool) {
	b.total++

	if !retain {
		return
	}

	if record.queryID == "" {
		b.unidentified++

		return
	}

	if record.reason != "" {
		b.rejected++
		b.refused[record.queryID] = record.reason

		return
	}

	b.seen[record.queryID]++

	if existing, ok := b.byID[record.queryID]; ok {
		if record.duration <= existing.duration {
			b.dropped++

			return
		}

		b.remove(existing)
		b.dropped++
	}

	b.byID[record.queryID] = record
	b.retained = append(b.retained, record)
	b.bytes += record.size()

	for b.bytes > MaxRetainedBindBytes && len(b.retained) > 0 {
		b.remove(b.retained[0])
		b.dropped++
	}
}

func (b *bindStore) remove(record *bindRecord) {
	for i, held := range b.retained {
		if held == record {
			b.retained = append(b.retained[:i], b.retained[i+1:]...)
			b.bytes -= record.size()

			break
		}
	}

	if b.byID[record.queryID] == record {
		delete(b.byID, record.queryID)
	}
}

// attach hands each unclaimed candidate its record, joining on the query identifier
// and nothing else, or says why there is none. gate is the sample's reason the
// literal tier is off, empty when it is on; a candidate already carrying a record from
// a sample the budget cut short keeps it.
func (b *bindStore) attach(candidates []*explainCandidate, gate string) {
	for _, c := range candidates {
		if c.mode != planModeNone || c.literal != nil || c.queryid == nil {
			continue
		}

		if gate != "" {
			c.literalReason = gate

			continue
		}

		id := strconv.FormatInt(*c.queryid, 10)

		if b.claimed[id] {
			// The same queryid under a different role. The record names an identifier
			// and nothing else, so the first claimant keeps it.
			c.literalReason = reasonBindClaimed

			continue
		}

		record, ok := b.byID[id]
		if !ok {
			c.literalReason = reasonNoBindRecord

			if reason, refused := b.refused[id]; refused {
				c.literalReason = reason
			}

			continue
		}

		b.claimed[id] = true
		b.remove(record)

		c.literal = record
		c.bindsSeen = b.seen[id]
	}
}
