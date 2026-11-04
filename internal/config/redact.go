package config

import (
	"flag"
	"regexp"
	"sort"
	"strings"
)

// redactedValue replaces the value of a sensitive flag in logs and help text.
const redactedValue = "<redacted>"

// inlineSensitiveFlagRe matches a sensitive flag and its value inside a single
// argument, e.g. ` -k user@key` within a -j value that Windows merged the
// following arguments into (a quoted path ending in a backslash).
var inlineSensitiveFlagRe = func() *regexp.Regexp {
	names := make([]string, 0, len(sensitiveFlags))
	for name := range sensitiveFlags {
		names = append(names, regexp.QuoteMeta(name))
	}
	sort.Strings(names)
	return regexp.MustCompile(`(^|\s)(--?(?:` + strings.Join(names, "|") + `))(=|\s+)("[^"]*"|\S+)`)
}()

// sensitiveFlagName returns the flag name if arg is a sensitive flag in any
// form the flag package accepts (-k, --k, -k=v, --k=v), and whether the
// value is attached with '='.
func sensitiveFlagName(arg string) (name string, attached bool, ok bool) {
	if !strings.HasPrefix(arg, "-") {
		return "", false, false
	}
	trimmed := strings.TrimPrefix(strings.TrimPrefix(arg, "-"), "-")
	name, _, attached = strings.Cut(trimmed, "=")
	return name, attached, sensitiveFlags[name]
}

// RedactArgs returns a copy of command-line args with the values of sensitive
// flags (see sensitiveFlags, e.g. -k) replaced, so they can be logged.
func RedactArgs(args []string) []string {
	out := make([]string, len(args))
	maskNext := false
	for i, arg := range args {
		if maskNext {
			out[i] = redactedValue
			maskNext = false
			continue
		}
		if name, attached, ok := sensitiveFlagName(arg); ok {
			if attached {
				dashes := arg[:len(arg)-len(strings.TrimLeft(arg, "-"))]
				out[i] = dashes + name + "=" + redactedValue
			} else {
				// "-k value": the value is the next argument.
				maskNext = true
				out[i] = arg
			}
			continue
		}
		out[i] = inlineSensitiveFlagRe.ReplaceAllString(arg, "${1}${2}${3}"+redactedValue)
	}
	return out
}

// RedactUsageDefaults hides the current values of sensitive flags in a flag
// set's help text. Flags are registered with their current values as
// defaults, so without this the usage printed after an argument error would
// show e.g. the -k API key as `(default "...")`. Only the help text changes;
// parsing must keep the real defaults so YAML values survive.
func RedactUsageDefaults(fs *flag.FlagSet) {
	for name := range sensitiveFlags {
		if f := fs.Lookup(name); f != nil && f.DefValue != "" {
			f.DefValue = redactedValue
		}
	}
}
