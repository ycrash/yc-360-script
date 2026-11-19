package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ArgumentsDocURL documents every yc-360 script argument.
const ArgumentsDocURL = "https://docs.ycrash.io/yc-360/all-yc-360-script-arguments.html"

// ArgumentsHelpHint points to -h and the docs.
func ArgumentsHelpHint() string {
	return fmt.Sprintf("Run %s -h or see %s for all arguments.", filepath.Base(os.Args[0]), ArgumentsDocURL)
}

// WithArgumentsHelpHint ends an argument error with ArgumentsHelpHint on a
// line of its own, after a blank line, so it stands apart from the error.
func WithArgumentsHelpHint(msg string) string {
	return strings.TrimSpace(msg) + "\n\n" + ArgumentsHelpHint()
}

// ErrHelp is returned by ParseFlags when -h or -help is passed.
var ErrHelp = flag.ErrHelp

// ArgError is a command-line argument the yc-360 script can't use, such as
// "-p" with no value. Msg says what is wrong and how to fix it.
type ArgError struct {
	Msg string
}

func (e *ArgError) Error() string { return e.Msg }

// The flag package's own error strings (src/flag/flag.go).
var (
	reNeedsValue   = regexp.MustCompile(`^flag needs an argument: -(.+)$`)
	reInvalidValue = regexp.MustCompile(`^invalid value "(.*)" for flag -([^:]+): (.*)$`)
	reInvalidBool  = regexp.MustCompile(`^invalid boolean value "(.*)" for -([^:]+): (.*)$`)
	reBoolFlag     = regexp.MustCompile(`^invalid boolean flag ([^:]+): (.*)$`)
	reUndefined    = regexp.MustCompile(`^flag provided but not defined: -(.+)$`)
	reBadSyntax    = regexp.MustCompile(`^bad flag syntax: (.*)$`)
)

// parseArgs parses args like flagSet.Parse, but instead of the flag
// package's terse error followed by the full help text, it returns an
// *ArgError that names the argument and says how to fix it.
func parseArgs(flagSet *flag.FlagSet, args []string) error {
	ArgWarnings = nil
	if err := checkValuesAreNotFlags(flagSet, args); err != nil {
		return err
	}

	// Keep the flag package from printing its own error and the help text.
	flagSet.SetOutput(io.Discard)
	err := flagSet.Parse(args)
	flagSet.SetOutput(nil) // back to stderr, for ShowUsage

	if errors.Is(err, flag.ErrHelp) {
		return err
	}
	if err != nil {
		return &ArgError{Msg: friendlyFlagError(flagSet, err)}
	}
	return checkLeftoverArgs(flagSet, args)
}

// ArgWarnings holds problems with the command line that don't stop the run,
// such as a stray word after the last argument. The CLI logs them once the
// logger writes to the log file.
var ArgWarnings []string

// checkLeftoverArgs handles words the flag package didn't read. The yc-360
// script takes no positional arguments, and the flag package stops at the
// first one, so a value with an unquoted space (-a First App) or a separate
// value for a true/false argument (-onlyCapture false) silently drops every
// argument after it. That is an error when arguments would be lost or a
// true/false argument would get the opposite of the value written;
// otherwise the stray words are only reported as a warning.
func checkLeftoverArgs(flagSet *flag.FlagSet, args []string) error {
	leftover := flagSet.Args()
	if len(leftover) == 0 {
		return nil
	}
	start := len(args) - len(leftover)
	if start > 0 && args[start-1] == "--" {
		return nil // "--" ends the arguments on purpose; nothing is lost by accident
	}

	// Words up to the next argument, and the arguments that were dropped.
	var stray, lost []string
	for i, a := range leftover {
		if name, _, ok := flagName(a); ok && flagSet.Lookup(name) != nil {
			lost = leftover[i:]
			break
		}
		stray = append(stray, a)
	}

	if err := checkMissingDash(flagSet, args, start, stray); err != nil {
		return err
	}

	hint := leftoverHint(flagSet, args, start, stray)
	strayText := strings.Join(stray, " ")

	if len(lost) > 0 {
		return &ArgError{Msg: fmt.Sprintf("%q is not an argument or the value of one, so the yc-360 script stopped reading arguments there and would ignore: %s. %s",
			strayText, strings.Join(lost, " "), hint)}
	}
	if boolName, ok := precedingBoolFlag(flagSet, args, start); ok {
		if b, err := strconv.ParseBool(stray[0]); err == nil && !b {
			return &ArgError{Msg: fmt.Sprintf("-%s %s turns -%s on, because a true/false argument doesn't take a separate value. %s",
				boolName, stray[0], boolName, hint)}
		}
	}
	ArgWarnings = append(ArgWarnings, fmt.Sprintf("Ignored %q: it is not an argument or the value of one. %s", strayText, hint))
	return nil
}

// checkMissingDash catches an argument written without its leading "-",
// e.g. "cmdTimeout 1s" in "-appLogLineCount -1 cmdTimeout 1s". The argument
// would be silently ignored, so it is an error. Right after a text value,
// only a camelCase name (cmdTimeout, onlyCapture) counts: a plain word such
// as "port" in "-a My port app" is more likely part of an unquoted app name.
func checkMissingDash(flagSet *flag.FlagSet, args []string, start int, stray []string) error {
	_, _, afterText := precedingValue(flagSet, args, start)
	for i, word := range stray {
		name, rest := word, ""
		if eq := strings.IndexByte(word, '='); eq > 0 {
			name, rest = word[:eq], word[eq:]
		}
		f := flagSet.Lookup(name)
		if f != nil && afterText && !isCamelCase(name) {
			f = nil
		}
		if f == nil && !afterText {
			if match := caseInsensitiveLookup(flagSet, name); match != "" {
				f = flagSet.Lookup(match)
			}
		}
		if f == nil {
			continue
		}
		fix := "-" + f.Name + rest
		if rest == "" && !isBoolFlag(f) {
			if i+1 < len(stray) {
				fix += " " + stray[i+1]
			} else {
				fix += " <value>"
			}
		}
		return &ArgError{Msg: fmt.Sprintf("%q is missing its leading \"-\", so the yc-360 script would ignore it. Write %s.%s",
			word, fix, aboutFlag(f))}
	}
	return nil
}

// isCamelCase reports whether name mixes lower and upper case, like
// cmdTimeout: such a word is an argument name, never ordinary text.
func isCamelCase(name string) bool {
	return strings.ToLower(name) != name && strings.ToUpper(name) != name
}

// leftoverHint says how to fix stray words that start at args[start].
func leftoverHint(flagSet *flag.FlagSet, args []string, start int, stray []string) string {
	if boolName, ok := precedingBoolFlag(flagSet, args, start); ok && len(stray) > 0 {
		if _, err := strconv.ParseBool(stray[0]); err == nil {
			return fmt.Sprintf("Write -%s=%s instead of -%s %s.", boolName, stray[0], boolName, stray[0])
		}
	}
	it, itIs := "it", "it is"
	if len(stray) > 1 {
		it, itIs = "them", "they are"
	}
	if name, value, ok := precedingValue(flagSet, args, start); ok {
		return fmt.Sprintf("If %s part of the -%s value, wrap the whole value in double quotes, for example -%s \"%s\".",
			itIs, name, name, strings.Join(append([]string{value}, stray...), " "))
	}
	return fmt.Sprintf("Remove %s, or wrap a value that contains spaces in double quotes.", it)
}

// precedingBoolFlag reports whether args[start-1] is a true/false argument
// written without "=", e.g. "-onlyCapture" in "-onlyCapture false".
func precedingBoolFlag(flagSet *flag.FlagSet, args []string, start int) (string, bool) {
	if start < 1 {
		return "", false
	}
	name, hasValue, ok := flagName(args[start-1])
	if !ok || hasValue {
		return "", false
	}
	f := flagSet.Lookup(name)
	return name, f != nil && isBoolFlag(f)
}

// precedingValue returns the text argument and value read just before
// args[start], e.g. "a" and "First" in "-a First App". Numbers, durations and
// true/false values never contain spaces, so they don't count.
func precedingValue(flagSet *flag.FlagSet, args []string, start int) (name, value string, ok bool) {
	if start >= 1 {
		if n, hasValue, isFlag := flagName(args[start-1]); isFlag && hasValue {
			if f := flagSet.Lookup(n); f != nil && isTextFlag(f) {
				return n, args[start-1][strings.IndexByte(args[start-1], '=')+1:], true
			}
		}
	}
	if start >= 2 {
		if n, hasValue, isFlag := flagName(args[start-2]); isFlag && !hasValue {
			if f := flagSet.Lookup(n); f != nil && isTextFlag(f) {
				return n, args[start-1], true
			}
		}
	}
	return "", "", false
}

// checkValuesAreNotFlags catches an argument whose value was left out but
// is followed by another argument, e.g. "-c -p 123". The flag package
// would silently take "-p" as the value of -c and then stop at "123", so
// the run would fail later with a confusing error.
func checkValuesAreNotFlags(flagSet *flag.FlagSet, args []string) error {
	for i := 0; i < len(args); i++ {
		name, hasValue, ok := flagName(args[i])
		if !ok {
			return nil // the flag package stops at the first non-flag argument
		}
		if hasValue {
			continue
		}
		f := flagSet.Lookup(name)
		if f == nil || isBoolFlag(f) || i+1 >= len(args) {
			continue // unknown or missing values are reported by Parse
		}
		next := args[i+1]
		if nextName, _, ok := flagName(next); ok && flagSet.Lookup(nextName) != nil {
			return &ArgError{Msg: fmt.Sprintf("The -%s argument needs a value, but it was followed by %s. %s%s",
				name, next, putValueHint(name), aboutFlag(f))}
		}
		i++ // skip the value
	}
	return nil
}

// flagName returns the name in "-name", "--name" or "-name=value", the
// way the flag package reads it. ok is false for "--", "-" and non-flags.
func flagName(arg string) (name string, hasValue, ok bool) {
	if len(arg) < 2 || arg[0] != '-' || arg == "--" {
		return "", false, false
	}
	name = arg[1:]
	if name[0] == '-' {
		name = name[1:]
	}
	if name == "" || name[0] == '-' || name[0] == '=' {
		return "", false, false
	}
	if i := strings.IndexByte(name, '='); i >= 0 {
		return name[:i], true, true
	}
	return name, false, true
}

// isTextFlag reports whether f takes free text (a name, path or token) that
// may contain spaces, as opposed to a number, duration or true/false.
func isTextFlag(f *flag.Flag) bool {
	if isBoolFlag(f) {
		return false
	}
	if _, ok := f.Value.(*Duration); ok {
		return false
	}
	if g, ok := f.Value.(flag.Getter); ok {
		_, isString := g.Get().(string)
		return isString
	}
	return true // the list types (-appLogs, -accessLog, ...) hold paths and tokens
}

func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}

func friendlyFlagError(flagSet *flag.FlagSet, err error) string {
	msg := err.Error()

	if m := reNeedsValue.FindStringSubmatch(msg); m != nil {
		name := m[1]
		return fmt.Sprintf("The -%s argument needs a value, but none was passed. %s%s",
			name, putValueHint(name), aboutFlag(flagSet.Lookup(name)))
	}
	if m := reInvalidValue.FindStringSubmatch(msg); m != nil {
		value, name, reason := unquote(m[1]), m[2], m[3]
		f := flagSet.Lookup(name)
		return fmt.Sprintf("The -%s value %q is not valid: %s.%s", name, value, valueRule(f, reason), aboutFlag(f))
	}
	if m := reInvalidBool.FindStringSubmatch(msg); m != nil {
		value, name := unquote(m[1]), m[2]
		return fmt.Sprintf("The -%s value %q is not valid: it must be true or false, for example -%s or -%s=false.%s",
			name, value, name, name, aboutFlag(flagSet.Lookup(name)))
	}
	if m := reBoolFlag.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("The -%s argument is not valid: %s.%s", m[1], m[2], aboutFlag(flagSet.Lookup(m[1])))
	}
	if m := reUndefined.FindStringSubmatch(msg); m != nil {
		name := m[1]
		if match := caseInsensitiveLookup(flagSet, name); match != "" {
			return fmt.Sprintf("-%s is not a yc-360 script argument. Did you mean -%s? Argument names are case-sensitive.", name, match)
		}
		return fmt.Sprintf("-%s is not a yc-360 script argument. Check the spelling.", name)
	}
	if m := reBadSyntax.FindStringSubmatch(msg); m != nil {
		return fmt.Sprintf("%q is not a valid argument. Arguments start with - followed by the name, for example -p 12345.", m[1])
	}
	return msg + "."
}

func putValueHint(name string) string {
	return fmt.Sprintf("Put the value right after -%s, and wrap it in double quotes if it contains spaces.", name)
}

// aboutFlag returns " About -name: <help text>." or "" for an unknown flag.
func aboutFlag(f *flag.Flag) string {
	if f == nil || strings.TrimSpace(f.Usage) == "" {
		return ""
	}
	usage := strings.TrimSpace(f.Usage)
	if !strings.HasSuffix(usage, ".") {
		usage += "."
	}
	return fmt.Sprintf(" About -%s: %s", f.Name, usage)
}

// valueRule turns the flag package's "parse error" for a number into the
// rule the value breaks; any other reason (e.g. from a duration) is kept.
func valueRule(f *flag.Flag, reason string) string {
	if f != nil {
		if _, ok := f.Value.(*Duration); ok {
			return "it must be a duration such as 30s, 5m or 1h30m"
		}
		if g, ok := f.Value.(flag.Getter); ok {
			switch g.Get().(type) {
			case int, int64:
				if reason == "value out of range" {
					return "the number is too large"
				}
				return "it must be a whole number"
			case uint, uint64:
				if reason == "value out of range" {
					return "the number is too large"
				}
				return "it must be zero or a positive whole number"
			}
		}
	}
	return strings.TrimSuffix(reason, ".")
}

// configFileError explains a -c file that can't be opened.
func configFileError(path string, err error) error {
	where := ""
	if !filepath.IsAbs(path) {
		if dir, e := os.Getwd(); e == nil {
			where = fmt.Sprintf(" A relative path is looked up in %s.", dir)
		}
	}
	reason := strings.TrimSuffix(err.Error(), ".")
	return &ArgError{Msg: fmt.Sprintf("The config file passed with -c can't be read: %s. Check the path passed with -c.%s", reason, where)}
}

// unquote undoes the %q escaping in the flag package's messages, so a
// Windows path isn't shown with doubled backslashes after we quote it again.
func unquote(s string) string {
	if u, err := strconv.Unquote(`"` + s + `"`); err == nil {
		return u
	}
	return s
}

func caseInsensitiveLookup(flagSet *flag.FlagSet, name string) string {
	match := ""
	flagSet.VisitAll(func(f *flag.Flag) {
		if match == "" && strings.EqualFold(f.Name, name) {
			match = f.Name
		}
	})
	return match
}
