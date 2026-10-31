package mysql

import (
	"regexp"
	"strings"
)

// redactedValue replaces one value; the same token the postgres capture writes.
const redactedValue = "<redacted>"

// redactStatement replaces literal values in SQL text, keeping names, keywords,
// placeholders, @variables, comments and hints; /*! */ bodies are read as SQL. A
// credential-setting statement is replaced past its first two words. Returns the
// text and the number of values replaced.
//
// Double-quoted text is always replaced (ANSI_QUOTES is unknown). A literal whose
// end depends on NO_BACKSLASH_ESCAPES, or text cut mid-literal, is replaced to the end.
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

// credentialStatement reports a credential-setting statement and the head it keeps.
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
			// MySQL allows names starting with a digit, like 1st_quarter.
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

// redactedLiteral replaces the literal opening s, keeping its quotes.
func redactedLiteral(s string, quote byte) string {
	n := literalLength(s, quote)
	closed := n >= 2 && s[n-1] == quote

	if closed {
		return string(quote) + redactedValue + string(quote)
	}

	return string(quote) + redactedValue
}

// literalLength is the length of the literal opening s. If it ends differently with
// and without NO_BACKSLASH_ESCAPES ('C:\dir\', 'O\'Brien'), it runs to the end.
func literalLength(s string, quote byte) int {
	n := quotedLength(s, quote, true)
	if quotedLength(s, quote, false) != n {
		return len(s)
	}

	return n
}

// literalPrefix is the length of a literal prefix: X, B, N, or an introducer like _utf8mb4.
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

// quotedLength is the length of the quoted text opening s, quotes included;
// unterminated, it runs to the end.
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

// numberLength is the length of the number opening s, including 0x and 0b forms.
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

// lineComment: -- starts a comment only before whitespace or a control character.
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

// isNameByte: letters, digits, _, $ and multi-byte character bytes.
func isNameByte(c byte) bool {
	return c == '_' || c == '$' || isDigit(c) || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// recordField is a record field in an InnoDB lock report; its hex and text are row values.
//
//	0: len 8; hex 8000000000000001; asc         ;;
var recordField = regexp.MustCompile(`^(\s*\d+: len \d+; hex )\S*(; asc ).*$`)

// monitorStatementEnd are line prefixes that end a transaction's statement in InnoDB monitor output.
var monitorStatementEnd = []string{
	"RECORD LOCKS", "TABLE LOCK", "------- TRX", "---TRANSACTION", "*** ", "TRANSACTION ",
	"Trx read view", "Record lock", "LOCK WAIT", "mysql tables in use", "MySQL thread id", "MariaDB thread id",
	"--------", "Hold", "TOO DEEP", "WE ROLL BACK", "Trx #rec",
}

// redactMonitorText redacts InnoDB monitor output (SHOW ENGINE INNODB STATUS,
// logged deadlocks): statements after a thread line and record field values.
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

		if isMonitorThreadLine(trimmed) {
			inStatement = true

			continue
		}

		if !inStatement {
			continue
		}

		if statementEnds(trimmed) || hasLogPrefix(line) {
			inStatement = isMonitorThreadLine(trimmed)

			continue
		}

		var n int

		lines[i], n = redactStatement(line)
		redacted += n
	}

	return strings.Join(lines, "\n"), redacted
}

// redactServerMessage redacts a replica's Last_*_Error. Values are embedded
// unescaped, so quotes don't pair:
//
//	Error 'Duplicate entry '42' for key 'orders.PRIMARY'' on query. Default database: 'app'. Query: 'INSERT ... VALUES (42, 'x')'
//
// Everything from the first quote to the last is replaced (to the end if the count
// is odd). Messages quoting only GTIDs are kept; a trailing Query: is redacted as SQL.
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

// messageQuery opens the statement in a statement-based failure.
const messageQuery = "'. Query: '"

// transactionName matches a GTID (tagged or not) or ANONYMOUS.
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

// messageQuotes are the offsets of quotes in s, skipping apostrophes between letters.
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

// isMonitorThreadLine reports the line that opens a transaction's statement (MySQL or MariaDB).
func isMonitorThreadLine(line string) bool {
	return strings.HasPrefix(line, "MySQL thread id") || strings.HasPrefix(line, "MariaDB thread id")
}

func statementEnds(line string) bool {
	for _, prefix := range monitorStatementEnd {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}

	return false
}

// hasPlaceholder reports a ? outside literals, quoted names and comments.
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
