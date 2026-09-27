package postgres

import (
	"regexp"
	"strings"
)

// planExpressionLabels are the plan properties whose value is an expression or SQL
// text, where a statement's constants appear. Every other property is a node's name, a
// figure such as a cost or a row count, or a setting, and is never touched.
var planExpressionLabels = []string{
	"Filter", "Index Cond", "Recheck Cond", "Join Filter", "Hash Cond", "Merge Cond",
	"TID Cond", "One-Time Filter", "Run Condition", "Order By", "Sort Key", "Presorted Key",
	"Group Key", "Hash Key", "Cache Key", "Output", "Function Call", "Table Function Call",
	"Sampling", "Conflict Filter", "Window", "Remote SQL", "Query Text",
}

// planParametersLabel is auto_explain's list of the execution's bind values.
const planParametersLabel = "Query Parameters"

// planValue redacts one property's value.
func planValue(label, value string) (string, int) {
	if label == planParametersLabel {
		return redactParameterValues(value)
	}

	return redactLiterals(value)
}

// Each format's property line, built from the labels: text and YAML write a label as
// it is, JSON quotes it, and XML makes it a tag with its spaces as hyphens.
var (
	textPlanProperty = regexp.MustCompile(`^(\s*)(` + planLabelPattern(" ") + `): (.*)$`)
	yamlPlanProperty = regexp.MustCompile(`^(\s*(?:- )?)(` + planLabelPattern(" ") + `): (.*)$`)
	jsonPlanProperty = regexp.MustCompile(`^(\s*)"(` + planLabelPattern(" ") + `)": (.*)$`)
	xmlPlanProperty  = regexp.MustCompile(`^(\s*)<(` + planLabelPattern("-") + `)>(.*)$`)

	// yamlListItem is one element of a YAML list property, a JSON-escaped string.
	yamlListItem = regexp.MustCompile(`^(\s*- )(".*")$`)

	// planNode is a text plan's node line, which ends auto_explain's Query Text: every
	// node carries its costs.
	planNode = regexp.MustCompile(`\s\(cost=\d`)
)

func planLabelPattern(space string) string {
	labels := make([]string, 0, len(planExpressionLabels)+1)

	for _, label := range append([]string{planParametersLabel}, planExpressionLabels...) {
		labels = append(labels, regexp.QuoteMeta(strings.ReplaceAll(label, " ", space)))
	}

	return strings.Join(labels, "|")
}

// redactPlanEntry is an auto_explain entry's message: its first line says how long the
// execution took, and the plan follows.
func redactPlanEntry(text string) (string, int) {
	first, plan, found := strings.Cut(text, "\n")
	if !found {
		return text, 0
	}

	redacted, n := redactPlan(plan)
	if n == 0 {
		return text, 0
	}

	return first + "\n" + redacted, n
}

// redactPlan replaces the constants in a plan written in any of EXPLAIN's four formats,
// read from its first line: the agent's own EXPLAIN is text, and auto_explain can log
// each of them.
func redactPlan(plan string) (string, int) {
	lines := strings.Split(plan, "\n")

	out, redacted := planFormat(lines)(lines)
	if redacted == 0 {
		return plan, 0
	}

	return strings.Join(out, "\n"), redacted
}

// planFormat picks the format's redaction from the plan's first line.
func planFormat(lines []string) func([]string) ([]string, int) {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		switch {
		case strings.HasPrefix(trimmed, "{"), strings.HasPrefix(trimmed, "["):
			return redactJSONPlan

		case strings.HasPrefix(trimmed, "<"):
			return redactXMLPlan

		case strings.HasPrefix(trimmed, `Query Text: "`), strings.HasPrefix(trimmed, "- Plan:"),
			strings.HasPrefix(trimmed, "Plan:"):
			return redactYAMLPlan
		}

		break
	}

	return redactTextPlan
}

// redactTextPlan: a property is one line, except auto_explain's Query Text, whose SQL
// keeps its own line breaks and runs to the parameters or the plan's first node. A
// Query Text with no node after it is taken to run to the end.
func redactTextPlan(lines []string) ([]string, int) {
	out := make([]string, 0, len(lines))
	redacted := 0

	for i := 0; i < len(lines); i++ {
		match := textPlanProperty.FindStringSubmatch(lines[i])
		if match == nil {
			out = append(out, lines[i])

			continue
		}

		head, label, value := match[1]+match[2]+": ", match[2], match[3]

		if label == "Query Text" {
			end := i + 1
			for end < len(lines) && !planNode.MatchString(lines[end]) &&
				!strings.HasPrefix(strings.TrimSpace(lines[end]), planParametersLabel+": ") {
				end++
			}

			value = strings.Join(append([]string{value}, lines[i+1:end]...), "\n")
			i = end - 1
		}

		text, n := planValue(label, value)
		redacted += n

		out = append(out, strings.Split(head+text, "\n")...)
	}

	return out, redacted
}

// redactJSONPlan: every string on a listed property's line is one value, a list's
// elements included; the value is escaped again as EXPLAIN escapes it.
func redactJSONPlan(lines []string) ([]string, int) {
	out := make([]string, len(lines))
	redacted := 0

	for i, line := range lines {
		out[i] = line

		match := jsonPlanProperty.FindStringSubmatchIndex(line)
		if match == nil {
			continue
		}

		label := line[match[4]:match[5]]

		value, n := redactJSONStrings(line[match[6]:], func(s string) (string, int) { return planValue(label, s) })
		out[i] = line[:match[6]] + value
		redacted += n
	}

	return out, redacted
}

// redactJSONStrings rewrites each JSON string in s through redact and copies the rest.
func redactJSONStrings(s string, redact func(string) (string, int)) (string, int) {
	var out strings.Builder

	redacted := 0

	for {
		at := strings.IndexByte(s, '"')
		if at < 0 {
			out.WriteString(s)

			return out.String(), redacted
		}

		end, ok := jsonStringEnd([]byte(s), at)
		if !ok {
			out.WriteString(s)

			return out.String(), redacted
		}

		text, n := redact(unescapeJSON([]byte(s[at+1 : end-1])))

		out.WriteString(s[:at])

		if n > 0 {
			out.WriteString(escapeJSON(text))
		} else {
			out.WriteString(s[at:end])
		}

		redacted += n
		s = s[end:]
	}
}

// redactYAMLPlan: a listed property's value is a JSON-escaped string on its line, or,
// for a list, one on each of the lines after it.
func redactYAMLPlan(lines []string) ([]string, int) {
	out := make([]string, len(lines))
	redacted := 0
	list := ""

	for i, line := range lines {
		out[i] = line

		if list != "" {
			if item := yamlListItem.FindStringSubmatch(line); item != nil {
				value, n := redactJSONStrings(item[2], func(s string) (string, int) { return planValue(list, s) })
				out[i] = item[1] + value
				redacted += n

				continue
			}

			list = ""
		}

		match := yamlPlanProperty.FindStringSubmatch(line)
		if match == nil {
			continue
		}

		label, value := match[2], match[3]

		if strings.TrimSpace(value) == "" {
			list = label

			continue
		}

		redactedValue, n := redactJSONStrings(value, func(s string) (string, int) { return planValue(label, s) })
		out[i] = match[1] + label + ": " + redactedValue
		redacted += n
	}

	return out, redacted
}

// redactXMLPlan: a listed property's value is its element's text, which can run over
// lines (Query-Text), or, for a list, the text of each <Item> inside it.
func redactXMLPlan(lines []string) ([]string, int) {
	out := make([]string, 0, len(lines))
	redacted := 0

	for i := 0; i < len(lines); i++ {
		match := xmlPlanProperty.FindStringSubmatch(lines[i])
		if match == nil {
			out = append(out, lines[i])

			continue
		}

		indent, tag, rest := match[1], match[2], match[3]
		label := strings.ReplaceAll(tag, "-", " ")
		closing := "</" + tag + ">"

		// A list: an <Item> per element, then the closing tag on its own line.
		if strings.TrimSpace(rest) == "" && i+1 < len(lines) &&
			strings.HasPrefix(strings.TrimSpace(lines[i+1]), "<Item>") {
			out = append(out, lines[i])

			for i+1 < len(lines) && !strings.Contains(lines[i+1], closing) {
				i++

				item, n := redactXMLItem(lines[i], label)
				out = append(out, item)
				redacted += n
			}

			continue
		}

		// The element's text, up to its closing tag, however many lines on.
		end := i
		if !strings.Contains(rest, closing) {
			for end+1 < len(lines) {
				end++

				if strings.Contains(lines[end], closing) {
					break
				}
			}
		}

		element := strings.Join(append([]string{rest}, lines[i+1:end+1]...), "\n")

		value, after, found := strings.Cut(element, closing)
		if !found {
			after = ""
		}

		text, n := planValue(label, unescapeXML(value))
		if n == 0 {
			out = append(out, lines[i:end+1]...)
		} else {
			out = append(out, strings.Split(indent+"<"+tag+">"+escapeXML(text)+closing+after, "\n")...)
		}

		redacted += n
		i = end
	}

	return out, redacted
}

// redactXMLItem is one <Item> of a list property.
func redactXMLItem(line, label string) (string, int) {
	open := strings.Index(line, "<Item>")
	shut := strings.LastIndex(line, "</Item>")

	if open < 0 || shut < open {
		return line, 0
	}

	text, n := planValue(label, unescapeXML(line[open+len("<Item>"):shut]))
	if n == 0 {
		return line, 0
	}

	return line[:open+len("<Item>")] + escapeXML(text) + line[shut:], n
}

// escapeXML and unescapeXML are EXPLAIN's XML escaping and its reverse.
var (
	xmlEscaper   = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\r", "&#x0d;")
	xmlUnescaper = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&#x0d;", "\r", "&quot;", `"`, "&apos;", "'", "&amp;", "&")
)

func escapeXML(s string) string { return xmlEscaper.Replace(s) }

func unescapeXML(s string) string { return xmlUnescaper.Replace(s) }

// redactLiterals replaces the constants in SQL text or a plan's expression, keeping
// its shape: a string literal becomes '<redacted>' inside its own quotes, whatever
// prefix it has, a dollar-quoted one the same inside its tags, and a number
// <redacted>. Names, quoted identifiers, $n parameters, keywords such as NULL and
// true, comments, a cast's type with its modifiers, and the number a SubPlan or an
// InitPlan is known by all stay.
func redactLiterals(s string) (string, int) {
	var out strings.Builder

	redacted := 0

	for i := 0; i < len(s); {
		afterName := i > 0 && isNameByte(s[i-1])

		switch c := s[i]; {
		case c == '\'':
			out.WriteString("'" + redactedValue + "'")
			redacted++
			i += quotedLength(s[i:])

		case !afterName && stringPrefix(s[i:]) > 0:
			prefix := stringPrefix(s[i:])
			escapes := c == 'E' || c == 'e'

			out.WriteString(s[i:i+prefix] + "'" + redactedValue + "'")
			redacted++

			if escapes {
				i += prefix + escapedLength(s[i+prefix:])
			} else {
				i += prefix + quotedLength(s[i+prefix:])
			}

		case c == '"':
			n := quotedIdentifierLength(s[i:])
			out.WriteString(s[i : i+n])
			i += n

		case c == '$' && i+1 < len(s) && isDigit(s[i+1]):
			n := 1
			for i+n < len(s) && isDigit(s[i+n]) {
				n++
			}

			out.WriteString(s[i : i+n])
			i += n

		case c == '$' && !afterName && dollarTag(s[i:]) != "":
			tag := dollarTag(s[i:])

			body := s[i+len(tag):]
			end := strings.Index(body, tag)

			out.WriteString(tag + redactedValue + tag)
			redacted++

			if end < 0 {
				i = len(s)
			} else {
				i += len(tag) + end + len(tag)
			}

		case strings.HasPrefix(s[i:], "--"):
			n := strings.IndexByte(s[i:], '\n')
			if n < 0 {
				n = len(s) - i
			}

			out.WriteString(s[i : i+n])
			i += n

		case strings.HasPrefix(s[i:], "/*"):
			n := blockCommentLength(s[i:])
			out.WriteString(s[i : i+n])
			i += n

		case strings.HasPrefix(s[i:], "::"):
			n := 2 + castTypeLength(s[i+2:])
			out.WriteString(s[i : i+n])
			i += n

		case isDigit(c) || (c == '.' && i+1 < len(s) && isDigit(s[i+1])):
			out.WriteString(redactedValue)
			redacted++
			i += numberLength(s[i:])

		case isNameStart(c):
			n := 1
			for i+n < len(s) && isNameByte(s[i+n]) {
				n++
			}

			word := s[i : i+n]
			i += n

			// SubPlan 1 and InitPlan 1 are the plan's own names for its parts.
			if word == "SubPlan" || word == "InitPlan" {
				for i+1 < len(s) && s[i] == ' ' && isDigit(s[i+1]) {
					n = 1
					for i+n < len(s) && isDigit(s[i+n]) {
						n++
					}

					word += s[i : i+n]
					i += n
				}
			}

			out.WriteString(word)

		default:
			out.WriteByte(c)
			i++
		}
	}

	if redacted == 0 {
		return s, 0
	}

	return out.String(), redacted
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

func isNameByte(c byte) bool { return isNameStart(c) || isDigit(c) || c == '$' }

// stringPrefix is the length of E, B, X, N or U& when s opens a string literal with
// one, and 0 otherwise.
func stringPrefix(s string) int {
	if len(s) >= 2 && s[1] == '\'' && strings.IndexByte("EeBbXxNn", s[0]) >= 0 {
		return 1
	}

	if len(s) >= 3 && (s[0] == 'U' || s[0] == 'u') && s[1] == '&' && s[2] == '\'' {
		return 2
	}

	return 0
}

// escapedLength is the length of the E'…' literal s opens with, where a backslash
// escapes the character after it.
func escapedLength(s string) int {
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++

		case '\'':
			if i+1 < len(s) && s[i+1] == '\'' {
				i++

				continue
			}

			return i + 1
		}
	}

	return len(s)
}

// quotedIdentifierLength is the length of the quoted identifier s opens with, a doubled
// quote read as one inside it.
func quotedIdentifierLength(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}

		if i+1 < len(s) && s[i+1] == '"' {
			i++

			continue
		}

		return i + 1
	}

	return len(s)
}

// dollarTag is the $tag$ or $$ s opens with, or empty.
func dollarTag(s string) string {
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == '$':
			return s[:i+1]

		case isNameStart(c) || (i > 1 && isDigit(c)):
			continue
		}

		return ""
	}

	return ""
}

// blockCommentLength is the length of the comment s opens with; they nest.
func blockCommentLength(s string) int {
	depth := 0

	for i := 0; i+1 < len(s); i++ {
		switch s[i : i+2] {
		case "/*":
			depth++
			i++

		case "*/":
			depth--
			i++

			if depth == 0 {
				return i + 1
			}
		}
	}

	return len(s)
}

// numberLength is the length of the numeric literal s opens with: digits with
// underscores, a fraction, an exponent, or a 0x, 0o or 0b integer.
func numberLength(s string) int {
	if len(s) > 2 && s[0] == '0' && strings.IndexByte("xXoObB", s[1]) >= 0 {
		i := 2
		for i < len(s) && (isDigit(s[i]) || strings.IndexByte("abcdefABCDEF_", s[i]) >= 0) {
			i++
		}

		return i
	}

	i := 0
	for i < len(s) && (isDigit(s[i]) || s[i] == '_') {
		i++
	}

	if i < len(s) && s[i] == '.' && (i+1 >= len(s) || s[i+1] != '.') {
		i++

		for i < len(s) && (isDigit(s[i]) || s[i] == '_') {
			i++
		}
	}

	if i+1 < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if s[j] == '+' || s[j] == '-' {
			j++
		}

		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}

			i = j
		}
	}

	return i
}

// castTypeWords are the words a type's name continues with after a space, as
// format_type writes them: character varying, double precision, timestamp with time
// zone.
var castTypeWords = []string{"varying", "precision", "with", "without", "time", "zone", "local"}

// castTypeLength is the length of the type after a cast's "::": its name, qualified or
// quoted, its modifiers, such as varchar(20)'s or numeric(10,2)'s, and array brackets.
func castTypeLength(s string) int {
	i := 0

	for {
		switch {
		case i < len(s) && s[i] == '"':
			i += quotedIdentifierLength(s[i:])

		case i < len(s) && isNameStart(s[i]):
			for i < len(s) && isNameByte(s[i]) {
				i++
			}

		default:
			return i
		}

		for {
			switch {
			case i < len(s) && s[i] == '.':
				i++

				if i < len(s) && s[i] == '"' {
					i += quotedIdentifierLength(s[i:])
				} else {
					for i < len(s) && isNameByte(s[i]) {
						i++
					}
				}

				continue

			case i < len(s) && s[i] == '(':
				if n := typeModifierLength(s[i:]); n > 0 {
					i += n

					continue
				}

			case strings.HasPrefix(s[i:], "[]"):
				i += 2

				continue
			}

			break
		}

		next := ""

		for _, word := range castTypeWords {
			if strings.HasPrefix(s[i:], " "+word) &&
				(i+1+len(word) == len(s) || !isNameByte(s[i+1+len(word)])) {
				next = word

				break
			}
		}

		if next == "" {
			return i
		}

		i++
	}
}

// typeModifierLength is the length of a "(20)" or "(10,2)" s opens with, or 0.
func typeModifierLength(s string) int {
	for i := 1; i < len(s); i++ {
		switch c := s[i]; {
		case c == ')':
			if i == 1 {
				return 0
			}

			return i + 1

		case isDigit(c), c == ',', c == ' ':
			continue
		}

		return 0
	}

	return 0
}
