package postgres

import (
	"regexp"
	"strings"
)

// explainExecuteArguments matches the literal tier's EXPLAIN EXECUTE argument list,
// which track_utility = on stores verbatim - the application's bind values.
var explainExecuteArguments = regexp.MustCompile(
	`(?s)(EXECUTE\s+` + preparedStatementPrefix + `[0-9]+\s*)\(.*$`)

// redactExplainArguments keeps the statement name and replaces the arguments.
func redactExplainArguments(query string) (string, int) {
	if !explainExecuteArguments.MatchString(query) {
		return query, 0
	}

	return explainExecuteArguments.ReplaceAllString(query, "${1}(<redacted>)"), 1
}

// queryCell caps a statement, then replaces its credentials, so a literal the cap split is
// still found; the cap's "..." goes back on after, where no replacement can swallow it.
// The agent's own EXPLAIN EXECUTE arguments are replaced first, whole.
func queryCell(query string) (string, bool, int) {
	query, explained := redactExplainArguments(query)

	capped := truncateRunes(query, DefaultMaxQueryText)
	cut := capped != query

	if cut {
		capped = strings.TrimSuffix(capped, "...")
	}

	capped, redacted := redactCredentials(capped)

	if cut {
		capped += "..."
	}

	return capped, cut, redacted + explained
}

// redactCredentials replaces the string after a word ending in "password" (PASSWORD '…',
// password = '…') and, inside any literal, a connection string's password; the rest stays.
func redactCredentials(s string) (string, int) {
	return redactCredentialText(s, false)
}

// redactCredentialText scans one depth. A literal's body is scanned again, as a DO body is
// SQL; inside one, "password=" takes libpq's unquoted values too, and a URI is read.
func redactCredentialText(s string, inLiteral bool) (string, int) {
	var out strings.Builder

	redacted := 0

	// afterPassword: the last token was a word ending in "password", maybe then "=".
	afterPassword, afterEquals := false, false

	for i := 0; i < len(s); {
		afterName := i > 0 && isNameByte(s[i-1])
		c := s[i]

		switch {
		case strings.IndexByte(" \t\n\v\f\r", c) >= 0:
			out.WriteByte(c)
			i++

			continue

		case inLiteral && afterPassword && afterEquals:
			out.WriteString(redactedValue)
			redacted++
			i += libpqValueLength(s[i:])

		case strings.HasPrefix(s[i:], "--"):
			n := strings.IndexByte(s[i:], '\n')
			if n < 0 {
				n = len(s) - i
			}

			out.WriteString(s[i : i+n])
			i += n

			continue

		case strings.HasPrefix(s[i:], "/*"):
			n := blockCommentLength(s[i:])
			out.WriteString(s[i : i+n])
			i += n

			continue

		case c == '=' && afterPassword && !afterEquals:
			out.WriteByte(c)
			i++
			afterEquals = true

			continue

		case c == '\'' || (!afterName && stringPrefix(s[i:]) > 0) ||
			(c == '$' && !afterName && dollarTag(s[i:]) != ""):
			lit := scanLiteral(s[i:])

			if afterPassword {
				out.WriteString(lit.encode(redactedValue))
				redacted++
			} else if body, n := redactCredentialText(lit.decoded(), true); n > 0 {
				out.WriteString(lit.encode(body))
				redacted += n
			} else {
				out.WriteString(s[i : i+lit.length])
			}

			i += lit.length

		case c == '"':
			n := quotedIdentifierLength(s[i:])
			name := strings.ReplaceAll(strings.TrimSuffix(s[i+1:i+n], `"`), `""`, `"`)

			out.WriteString(s[i : i+n])
			i += n

			afterPassword, afterEquals = endsWithPassword(name), false

			continue

		case c == '$' && i+1 < len(s) && isDigit(s[i+1]):
			n := 1
			for i+n < len(s) && isDigit(s[i+n]) {
				n++
			}

			out.WriteString(s[i : i+n])
			i += n

		case inLiteral && !afterName && connectionURIAt(s[i:]):
			n := strings.IndexAny(s[i:], " \t\n\v\f\r")
			if n < 0 {
				n = len(s) - i
			}

			hidden, k := redactURIPasswords(s[i : i+n])
			out.WriteString(hidden)
			redacted += k
			i += n

		case isNameStart(c):
			n := 1
			for i+n < len(s) && isNameByte(s[i+n]) {
				n++
			}

			out.WriteString(s[i : i+n])
			afterPassword, afterEquals = endsWithPassword(s[i:i+n]), false
			i += n

			continue

		default:
			out.WriteByte(c)
			i++
		}

		afterPassword, afterEquals = false, false
	}

	if redacted == 0 {
		return s, 0
	}

	return out.String(), redacted
}

func endsWithPassword(word string) bool {
	return strings.HasSuffix(strings.ToLower(word), "password")
}

// connectionURIAt is whether s opens with a libpq URI's scheme.
func connectionURIAt(s string) bool {
	lower := strings.ToLower(s[:min(len(s), len("postgresql://"))])

	return strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://")
}

// libpqValueLength reads a quoted value both as libpq and as SQL and keeps the longer, so
// neither a backslash-escaped quote nor a doubled one leaves part of it behind.
func libpqValueLength(s string) int {
	n := keywordValueLength(s)

	if strings.HasPrefix(s, "'") {
		sqlLength, _ := quotedExtent(s, false)
		n = max(n, sqlLength)
	}

	return n
}

// sqlLiteral is one string literal: its prefix (E, U&, …), its quote or $tag$, its body.
type sqlLiteral struct {
	prefix string
	quote  string
	body   string
	length int

	// escapes: E'…', where a backslash escapes the character after it.
	escapes bool

	// closed is false for a literal the end of the text cut off.
	closed bool
}

// scanLiteral reads the literal s opens with: '…', E'…', B'…', X'…', N'…', U&'…' or $tag$.
func scanLiteral(s string) sqlLiteral {
	if s[0] == '$' {
		tag := dollarTag(s)
		body := s[len(tag):]

		end := strings.Index(body, tag)
		if end < 0 {
			return sqlLiteral{quote: tag, body: body, length: len(s)}
		}

		return sqlLiteral{quote: tag, body: body[:end], length: len(tag) + end + len(tag), closed: true}
	}

	prefix := s[:stringPrefix(s)]
	escapes := prefix == "E" || prefix == "e"

	n, closed := quotedExtent(s[len(prefix):], escapes)

	body := s[len(prefix)+1 : len(prefix)+n]
	if closed {
		body = body[:len(body)-1]
	}

	return sqlLiteral{
		prefix: prefix, quote: "'", body: body,
		length: len(prefix) + n, escapes: escapes, closed: closed,
	}
}

// quotedExtent is the length of the '…' literal s opens with and whether a quote closes it;
// a doubled quote is one quote, and with escapes a backslash escapes the next character.
func quotedExtent(s string, escapes bool) (int, bool) {
	for i := 1; i < len(s); i++ {
		switch {
		case escapes && s[i] == '\\':
			i++

		case s[i] == '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++

				continue
			}

			return i + 1, true
		}
	}

	return len(s), false
}

// decoded is the value as SQL reads it; backslash escapes other than \' stay as written,
// so encode restores them unchanged.
func (l sqlLiteral) decoded() string {
	if l.quote != "'" {
		return l.body
	}

	if !l.escapes {
		return strings.ReplaceAll(l.body, "''", "'")
	}

	var out strings.Builder

	for i := 0; i < len(l.body); i++ {
		switch {
		case l.body[i] == '\\' && i+1 < len(l.body):
			if l.body[i+1] == '\'' {
				out.WriteByte('\'')
			} else {
				out.WriteString(l.body[i : i+2])
			}

			i++

		case l.body[i] == '\'' && i+1 < len(l.body) && l.body[i+1] == '\'':
			out.WriteByte('\'')
			i++

		default:
			out.WriteByte(l.body[i])
		}
	}

	return out.String()
}

// encode writes body back as this literal, without a closing quote the text never had.
func (l sqlLiteral) encode(body string) string {
	if l.quote == "'" {
		body = strings.ReplaceAll(body, "'", "''")
	}

	end := ""
	if l.closed {
		end = l.quote
	}

	return l.prefix + l.quote + body + end
}
