package mysql

import (
	"regexp"
	"strings"
)

// redactedValue replaces one value; a string literal keeps its quotes and prefix
// around it. The same token the postgres capture writes.
const redactedValue = "<redacted>"

// redactStatement replaces the values in SQL text and keeps the statement: a string
// literal becomes '<redacted>' inside its own quotes, with whatever prefix it had
// (X, B, N, _charset), and a number, hex or bit value <redacted>. Names, `quoted`
// identifiers, keywords such as NULL and TRUE, ? placeholders, @variables, plain
// comments and optimizer hints stay. An executable comment /*! ... */ is SQL the
// server runs, so its body is read as SQL. A statement that sets a credential is
// replaced whole, past its first two words. It returns the text and the number of
// values replaced.
//
// MySQL's lexer, not PostgreSQL's: a double-quoted text is a string unless the
// application runs with ANSI_QUOTES, and is replaced either way - a name lost is
// better than a value kept; a backslash escapes inside a literal unless the
// session runs with NO_BACKSLASH_ESCAPES, which no statement records, so a literal
// whose end depends on it is replaced to the end (see literalLength); # starts a
// comment, and so does -- but only before a space or a control character, so
// 5--3 is arithmetic. Text cut mid-literal - query_sample_text stops at
// performance_schema_max_sql_text_length - is replaced to its end.
func redactStatement(s string) (string, int) {
	if head, ok := credentialStatement(s); ok {
		return head + " " + redactedValue, 1
	}

	return redactValues(s)
}

// credentialStatements open with the words of a statement that carries a password.
var credentialStatements = [][]string{
	{"CREATE", "USER"},
	{"ALTER", "USER"},
	{"SET", "PASSWORD"},
	{"CHANGE", "MASTER"},
	{"CHANGE", "REPLICATION"},
	{"START", "SLAVE"},
	{"START", "REPLICA"},
	{"START", "GROUP_REPLICATION"},
	{"CREATE", "SERVER"},
	{"ALTER", "SERVER"},
}

// credentialStatement reports a statement that sets a credential, and the text it
// keeps: everything before its words, and the words.
func credentialStatement(s string) (string, bool) {
	start := skipSpaceAndComments(s, 0)

	words := make([]string, 0, 2)
	at := start

	for len(words) < 2 && at < len(s) {
		n := nameLength(s[at:])
		if n == 0 {
			break
		}

		words = append(words, strings.ToUpper(s[at:at+n]))
		at = skipSpaceAndComments(s, at+n)
	}

	for _, statement := range credentialStatements {
		if len(words) == 2 && words[0] == statement[0] && words[1] == statement[1] {
			return strings.TrimRight(s[:at], " \t\r\n"), true
		}
	}

	return "", false
}

// skipSpaceAndComments steps over whitespace and plain comments from i.
func skipSpaceAndComments(s string, i int) int {
	for i < len(s) {
		switch {
		case isSpace(s[i]):
			i++

		case strings.HasPrefix(s[i:], "/*") && !strings.HasPrefix(s[i:], "/*!") && !strings.HasPrefix(s[i:], "/*+"):
			i += blockCommentLength(s[i:])

		case s[i] == '#' || lineComment(s[i:]):
			i += lineLength(s[i:])

		default:
			return i
		}
	}

	return i
}

func redactValues(s string) (string, int) {
	var out strings.Builder

	out.Grow(len(s))

	redacted := 0

	// executable counts the /*! comments open: their */ ends one, not a comment.
	executable := 0

	for i := 0; i < len(s); {
		afterName := i > 0 && isNameByte(s[i-1])
		c := s[i]

		switch {
		case executable > 0 && strings.HasPrefix(s[i:], "*/"):
			out.WriteString("*/")
			executable--
			i += 2

		case strings.HasPrefix(s[i:], "/*!"):
			// The version that gates the body is part of the comment, not a value.
			n := 3
			for i+n < len(s) && isDigit(s[i+n]) {
				n++
			}

			out.WriteString(s[i : i+n])
			executable++
			i += n

		case strings.HasPrefix(s[i:], "/*"):
			n := blockCommentLength(s[i:])
			out.WriteString(s[i : i+n])
			i += n

		case c == '#' || lineComment(s[i:]):
			n := lineLength(s[i:])
			out.WriteString(s[i : i+n])
			i += n

		case c == '`':
			n := quotedLength(s[i:], '`', false)
			out.WriteString(s[i : i+n])
			i += n

		case c == '\'' || c == '"':
			out.WriteString(redactedLiteral(s[i:], c))
			redacted++
			i += literalLength(s[i:], c)

		case !afterName && literalPrefix(s[i:]) > 0:
			prefix := literalPrefix(s[i:])
			quote := s[i+prefix]

			out.WriteString(s[i:i+prefix] + redactedLiteral(s[i+prefix:], quote))
			redacted++
			i += prefix + literalLength(s[i+prefix:], quote)

		case !afterName && (isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1]))):
			// A run of name bytes starting with a digit is a number, or a hex or bit
			// value, unless it is a name: MySQL allows names like 1st_quarter.
			n := numberLength(s[i:])
			if word := nameLength(s[i:]); word > n {
				out.WriteString(s[i : i+word])
				i += word

				continue
			}

			out.WriteString(redactedValue)
			redacted++
			i += n

		case c == '@':
			// @var and @@sysvar are names.
			n := 1
			for i+n < len(s) && (s[i+n] == '@' || isNameByte(s[i+n])) {
				n++
			}

			out.WriteString(s[i : i+n])
			i += n

		case isNameByte(c):
			n := nameLength(s[i:])
			if n == 0 {
				n = 1
			}

			out.WriteString(s[i : i+n])
			i += n

		default:
			out.WriteByte(c)
			i++
		}
	}

	return out.String(), redacted
}

// redactedLiteral is the replacement for the literal opening s: its quote,
// redactedValue, and the closing quote if the text has one.
func redactedLiteral(s string, quote byte) string {
	n := literalLength(s, quote)
	closed := n >= 2 && s[n-1] == quote

	if closed {
		return string(quote) + redactedValue + string(quote)
	}

	return string(quote) + redactedValue
}

// literalLength is the length of the literal opening s, read both ways a session
// can read it: with a backslash escaping the next character, and, under
// NO_BACKSLASH_ESCAPES, as itself. Where the two end it at different quotes, as
// they do 'C:\dir\' and 'O\'Brien', what one reading takes for SQL after it the
// other takes for values, and neither can be ruled out, so the literal runs to the
// end. Where they agree, as on '\\' and '\n', the rest reads the same either way.
func literalLength(s string, quote byte) int {
	n := quotedLength(s, quote, true)
	if quotedLength(s, quote, false) != n {
		return len(s)
	}

	return n
}

// literalPrefix is the length of a prefix that makes the quote after it a literal
// of another kind: X'..' and B'..' (hex and bit), N'..' (national), and an
// introducer such as _utf8mb4'..' or _binary'..'.
func literalPrefix(s string) int {
	if len(s) >= 2 && strings.IndexByte("XxBbNn", s[0]) >= 0 && (s[1] == '\'' || s[1] == '"') {
		return 1
	}

	if len(s) >= 2 && s[0] == '_' {
		n := nameLength(s)
		if n > 1 && n < len(s) && (s[n] == '\'' || s[n] == '"') {
			return n
		}
	}

	return 0
}

// quotedLength is the length of the quoted text opening s, its quotes included: a
// doubled quote is one character, and, in a literal, a backslash escapes the next.
// Unterminated, it runs to the end.
func quotedLength(s string, quote byte, backslash bool) int {
	for i := 1; i < len(s); i++ {
		switch {
		case backslash && s[i] == '\\':
			i++

		case s[i] == quote && i+1 < len(s) && s[i+1] == quote:
			i++

		case s[i] == quote:
			return i + 1
		}
	}

	return len(s)
}

// numberLength is the length of the number opening s: 0x and 0b values, digits
// with a fraction and an exponent.
func numberLength(s string) int {
	if len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X') && isHexDigit(s[2]) {
		n := 2
		for n < len(s) && isHexDigit(s[n]) {
			n++
		}

		return n
	}

	if len(s) > 2 && s[0] == '0' && (s[1] == 'b' || s[1] == 'B') && (s[2] == '0' || s[2] == '1') {
		n := 2
		for n < len(s) && (s[n] == '0' || s[n] == '1') {
			n++
		}

		return n
	}

	n := 0
	for n < len(s) && isDigit(s[n]) {
		n++
	}

	if n < len(s) && s[n] == '.' {
		n++
		for n < len(s) && isDigit(s[n]) {
			n++
		}
	}

	if n+1 < len(s) && (s[n] == 'e' || s[n] == 'E') {
		m := n + 1
		if s[m] == '+' || s[m] == '-' {
			m++
		}

		if m < len(s) && isDigit(s[m]) {
			for m < len(s) && isDigit(s[m]) {
				m++
			}

			n = m
		}
	}

	return n
}

// nameLength is the length of the unquoted name opening s, zero if none does.
func nameLength(s string) int {
	n := 0
	for n < len(s) && isNameByte(s[n]) {
		n++
	}

	return n
}

// lineComment: -- starts a comment only before whitespace, a control character, or
// the end.
func lineComment(s string) bool {
	return strings.HasPrefix(s, "--") && (len(s) == 2 || s[2] <= ' ')
}

// lineLength runs to the end of the line, its newline excluded.
func lineLength(s string) int {
	if n := strings.IndexByte(s, '\n'); n >= 0 {
		return n
	}

	return len(s)
}

// blockCommentLength runs to the comment's close, or the end.
func blockCommentLength(s string) int {
	if n := strings.Index(s[2:], "*/"); n >= 0 {
		return n + 4
	}

	return len(s)
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isHexDigit(c byte) bool { return isDigit(c) || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') }

// isNameByte: letters, digits, _ and $, and any byte of a multi-byte character,
// which MySQL allows in an unquoted name.
func isNameByte(c byte) bool {
	return c == '_' || c == '$' || isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// recordField is one field of a record InnoDB prints in a lock report: its bytes in
// hex and as text, which are the row's values.
//
//	0: len 8; hex 8000000000000001; asc         ;;
var recordField = regexp.MustCompile(`^(\s*\d+: len \d+; hex )\S*(; asc ).*$`)

// monitorStatementEnd opens the lines that follow a transaction's statement in
// InnoDB's monitor output and deadlock reports.
var monitorStatementEnd = []string{
	"RECORD LOCKS", "TABLE LOCK", "------- TRX", "---TRANSACTION", "*** ", "TRANSACTION ",
	"Trx read view", "Record lock", "LOCK WAIT", "mysql tables in use", "MySQL thread id",
	"--------", "Hold", "TOO DEEP", "WE ROLL BACK", "Trx #rec",
}

// redactMonitorText replaces the values in InnoDB's monitor output: SHOW ENGINE
// INNODB STATUS, and the deadlock reports innodb_print_all_deadlocks writes to the
// error log. A transaction's statement - the lines after its "MySQL thread id"
// line - keeps its shape with its values replaced, and every record field's hex and
// text are replaced. Every other line is InnoDB's own counters and names, kept.
func redactMonitorText(text string) (string, int) {
	lines := strings.Split(text, "\n")
	redacted := 0
	inStatement := false

	for i, line := range lines {
		if m := recordField.FindStringSubmatch(line); m != nil {
			lines[i] = m[1] + redactedValue + m[2] + redactedValue + ";;"
			redacted++
			inStatement = false

			continue
		}

		trimmed := strings.TrimLeft(line, " \t")

		if strings.HasPrefix(trimmed, "MySQL thread id") {
			inStatement = true

			continue
		}

		if !inStatement {
			continue
		}

		if statementEnds(trimmed) || hasLogPrefix(line) {
			inStatement = strings.HasPrefix(trimmed, "MySQL thread id")

			continue
		}

		var n int

		lines[i], n = redactStatement(line)
		redacted += n
	}

	return strings.Join(lines, "\n"), redacted
}

// redactServerMessage replaces the values in a server's error message: a replica's
// Last_Error, Last_SQL_Error and Last_IO_Error. It is not SQL, and its quotes do not
// pair. The server drops a value, a whole message or a whole statement into its
// template unescaped, as in this statement-based failure:
//
//	Error 'Duplicate entry '42' for key 'orders.PRIMARY'' on query. Default database: 'app'. Query: 'INSERT ... VALUES (42, 'x')'
//
// so a SQL lexer reading it leaves part of a value outside any string, and keeps
// it. A value is always inside the quotes, though, so everything from the first
// quote to the last is replaced; with an odd count, a value holding a quote or a
// message cut short, to the end. What lies outside - error codes, worker numbers,
// log files and positions, the table a row event was for - is the template's own
// and stays.
//
// Two shapes keep more. A message whose every quoted text is a GTID or ANONYMOUS,
// the parallel applier's "Worker 1 failed executing transaction '...'", is kept
// whole: it names the transaction to skip or repair. A statement-based failure
// ends in the statement as sent, whose own quotes are escaped, and that is read as
// SQL, keeping its words with its values replaced.
func redactServerMessage(s string) (string, int) {
	i := strings.Index(s, messageQuery)
	if i < 0 {
		return redactMessageText(s)
	}

	head, headRedacted := redactMessageText(s[:i+1])

	statement := s[i+len(messageQuery):]
	closing := ""

	if strings.HasSuffix(statement, "'") {
		statement, closing = statement[:len(statement)-1], "'"
	}

	statement, statementRedacted := redactStatement(statement)

	return head + messageQuery[1:] + statement + closing, headRedacted + statementRedacted
}

// messageQuery opens the statement in a statement-based failure, after the default
// database's closing quote: "... Default database: 'app'. Query: '...'".
const messageQuery = "'. Query: '"

// transactionName is a quoted text a message may keep: one GTID, tagged or not, or
// ANONYMOUS, a transaction without one.
var transactionName = regexp.MustCompile(`^(?:ANONYMOUS|[0-9A-Fa-f]{8}(?:-[0-9A-Fa-f]{4}){3}-[0-9A-Fa-f]{12}(?::[A-Za-z_][A-Za-z0-9_]{0,31})?:[0-9]+)$`)

func redactMessageText(s string) (string, int) {
	quotes := messageQuotes(s)
	if len(quotes) == 0 {
		return s, 0
	}

	first, last := quotes[0], quotes[len(quotes)-1]

	if len(quotes)%2 == 1 {
		return s[:first+1] + redactedValue, 1
	}

	names := true

	for i := 0; i < len(quotes); i += 2 {
		names = names && transactionName.MatchString(s[quotes[i]+1:quotes[i+1]])
	}

	if names {
		return s, 0
	}

	return s[:first+1] + redactedValue + s[last:], 1
}

// messageQuotes are the offsets of the quotes in s that open or close a quoted
// text. An apostrophe between two letters - "the event's source log", "O'Brien" -
// is a word's, not a quote. One misread the other way only widens what is replaced.
func messageQuotes(s string) []int {
	var quotes []int

	for i := range len(s) {
		if s[i] != '\'' {
			continue
		}

		if i > 0 && i+1 < len(s) && isLetter(s[i-1]) && isLetter(s[i+1]) {
			continue
		}

		quotes = append(quotes, i)
	}

	return quotes
}

func isLetter(c byte) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func statementEnds(line string) bool {
	for _, prefix := range monitorStatementEnd {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// hasPlaceholder reports a ? marker outside literals, quoted names and comments: a
// prepared statement's text, which no server can run as written.
func hasPlaceholder(s string) bool {
	for i := 0; i < len(s); {
		afterName := i > 0 && isNameByte(s[i-1])

		switch c := s[i]; {
		case strings.HasPrefix(s[i:], "/*!"):
			i += 3

		case strings.HasPrefix(s[i:], "/*"):
			i += blockCommentLength(s[i:])

		case c == '#' || lineComment(s[i:]):
			i += lineLength(s[i:])

		case c == '`':
			i += quotedLength(s[i:], '`', false)

		case c == '\'' || c == '"':
			i += quotedLength(s[i:], c, true)

		case !afterName && literalPrefix(s[i:]) > 0:
			prefix := literalPrefix(s[i:])
			i += prefix + quotedLength(s[i+prefix:], s[i+prefix], true)

		case c == '?':
			return true

		default:
			i++
		}
	}

	return false
}
