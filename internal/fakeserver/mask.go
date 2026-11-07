package fakeserver

import (
	"bytes"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// masker finds the API key and the secrets, and keeps them out of everything the
// fake writes except the bodies it stores.
type masker struct {
	key     string
	secrets []string // as given, so an index names the same secret as Options.Secrets
	needles []needle // longest first, so a secret inside another is masked as the longer
}

type needle struct {
	value string
	label string
}

func newMasker(key string, secrets []string) *masker {
	m := &masker{key: key, secrets: secrets}
	if key != "" {
		m.needles = append(m.needles, needle{key, "<api-key>"})
	}

	for i, s := range secrets {
		if s != "" {
			m.needles = append(m.needles, needle{s, fmt.Sprintf("<secret-%d>", i+1)})
		}
	}

	sort.SliceStable(m.needles, func(i, j int) bool { return len(m.needles[i].value) > len(m.needles[j].value) })

	return m
}

// forms are the ways a value can appear in a URL: as itself, or escaped.
func forms(v string) []string {
	out := []string{v}
	for _, f := range []string{url.QueryEscape(v), url.PathEscape(v)} {
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}

	return out
}

func inAny(texts []string, v string) bool {
	for _, f := range forms(v) {
		for _, t := range texts {
			if strings.Contains(t, f) {
				return true
			}
		}
	}

	return false
}

func (m *masker) keyIn(texts []string) bool {
	return m.key != "" && inAny(texts, m.key)
}

// secretsIn returns the index of each secret found in texts.
func (m *masker) secretsIn(texts []string) []int {
	var found []int

	for i, s := range m.secrets {
		if s != "" && inAny(texts, s) {
			found = append(found, i)
		}
	}

	return found
}

func (m *masker) mask(s string) string {
	for _, n := range m.needles {
		for _, f := range forms(n.value) {
			s = strings.ReplaceAll(s, f, n.label)
		}
	}

	return s
}

func (m *masker) maskValues(q url.Values) url.Values {
	out := make(url.Values, len(q))
	for k, values := range q {
		masked := make([]string, len(values))
		for i, v := range values {
			masked[i] = m.mask(v)
		}

		out[m.mask(k)] = masked
	}

	return out
}

// maskHeader masks the secrets in every header and replaces the ApiKey value.
func (m *masker) maskHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for name, values := range h {
		masked := make([]string, len(values))
		for i, v := range values {
			switch {
			case name == "Apikey" && v != "":
				masked[i] = "<masked>"
			default:
				masked[i] = m.mask(v)
			}
		}

		out[m.mask(name)] = masked
	}

	return out
}

// maskRawQuery masks a query as it arrived. If a key or secret survives in an
// encoding the masking missed, the whole query is withheld.
func (m *masker) maskRawQuery(raw string) string {
	masked := m.mask(raw)

	if m.anyIn(unescapeWhatIsValid(masked)) {
		return "<withheld: it holds the API key or a secret>"
	}

	return masked
}

// unescapeWhatIsValid decodes a query as url.QueryUnescape does, but leaves a
// malformed escape as it is instead of giving up on the whole query.
func unescapeWhatIsValid(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			b.WriteByte(' ')
		case s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		default:
			b.WriteByte(s[i])
		}
	}

	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}

func (m *masker) anyIn(s string) bool {
	for _, n := range m.needles {
		if strings.Contains(s, n.value) {
			return true
		}
	}

	return false
}

func (m *masker) scanner() *bodyScanner {
	b := &bodyScanner{found: make([]bool, len(m.secrets))}
	for i, s := range m.secrets {
		if s == "" {
			continue
		}

		b.secrets = append(b.secrets, []byte(s))
		b.index = append(b.index, i)
		b.keep = max(b.keep, len(s)-1)
	}

	return b
}

// bodyScanner looks for the secrets in a body as it streams past, keeping only
// enough of the previous chunk to find one split across two.
type bodyScanner struct {
	secrets [][]byte
	index   []int // each secret's index in Options.Secrets
	found   []bool
	keep    int
	tail    []byte
	edge    []byte
}

func (b *bodyScanner) Write(p []byte) (int, error) {
	if len(b.secrets) == 0 {
		return len(p), nil
	}

	head := p[:min(len(p), b.keep)]
	b.edge = append(append(b.edge[:0], b.tail...), head...)

	for i, s := range b.secrets {
		if !b.found[b.index[i]] && (bytes.Contains(p, s) || bytes.Contains(b.edge, s)) {
			b.found[b.index[i]] = true
		}
	}

	if len(p) >= b.keep {
		b.tail = append(b.tail[:0], p[len(p)-b.keep:]...)
	} else {
		b.tail = append(b.tail[:0], b.edge[max(0, len(b.edge)-b.keep):]...)
	}

	return len(p), nil
}

func (b *bodyScanner) foundSecrets() []int {
	var out []int

	for i, ok := range b.found {
		if ok {
			out = append(out, i)
		}
	}

	return out
}
