package config

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseFlagsArgumentErrors(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })

	cases := []struct {
		name string
		args []string
		want []string // substrings of the message
	}{
		{"value missing at the end", []string{"-c"},
			[]string{"The -c argument needs a value, but none was passed.", "Put the value right after -c", "About -c: The config file path to load."}},
		{"value missing after another flag", []string{"-c", "yc-config.yaml", "-p"},
			[]string{"The -p argument needs a value, but none was passed.", "About -p: The process ID"}},
		{"value missing, next argument taken as the value", []string{"-c", "-p", "123"},
			[]string{"The -c argument needs a value, but it was followed by -p."}},
		{"value missing before -name=value", []string{"-a", "-p=123"},
			[]string{"The -a argument needs a value, but it was followed by -p=123."}},
		{"not a number", []string{"-port", "abc"},
			[]string{`The -port value "abc" is not valid: it must be a whole number.`, "About -port: Port to serve API service."}},
		{"negative unsigned", []string{"-logFileMaxCount", "-3"},
			[]string{`The -logFileMaxCount value "-3" is not valid: it must be zero or a positive whole number.`}},
		{"bad boolean", []string{"-onlyCapture=maybe"},
			[]string{`The -onlyCapture value "maybe" is not valid: it must be true or false, for example -onlyCapture or -onlyCapture=false.`}},
		{"bad duration", []string{"-m3Frequency", "soon"},
			[]string{`The -m3Frequency value "soon" is not valid: it must be a duration such as 30s, 5m or 1h30m.`}},
		{"backslashes in the value are shown as typed", []string{"-port", `C:\x`},
			[]string{`The -port value "C:\\x" is not valid`}},
		{"bad -verifySSL", []string{"-verifySSL", "maybe"},
			[]string{`The -verifySSL value "maybe" is not valid: it must be true or false, for example -verifySSL false.`}},
		{"-cmd without -urlParams", []string{"-cmd", "x"},
			[]string{"-urlParams and -cmd must be passed in pairs, but 0 -urlParams and 1 -cmd were passed."}},
		{"missing -c file", []string{"-c", "does-not-exist.yaml"},
			[]string{"The config file passed with -c can't be read:", "does-not-exist.yaml", "A relative path is looked up in"}},
		{"unquoted space drops later arguments", []string{"-a", "First", "App", "-p", "123"},
			[]string{`"App" is not an argument or the value of one, so the yc-360 script stopped reading arguments there and would ignore: -p 123.`,
				`If it is part of the -a value, wrap the whole value in double quotes, for example -a "First App".`}},
		{"unquoted spaces with -name=value", []string{"-a=My", "First", "App", "-onlyCapture"},
			[]string{`"First App" is not an argument`, `If they are part of the -a value`, `for example -a "My First App".`}},
		{"separate value for a true/false argument drops later arguments", []string{"-onlyCapture", "true", "-p", "123"},
			[]string{`"true" is not an argument`, "would ignore: -p 123.", "Write -onlyCapture=true instead of -onlyCapture true."}},
		{"separate false value would turn the argument on", []string{"-p", "123", "-onlyCapture", "false"},
			[]string{"-onlyCapture false turns -onlyCapture on, because a true/false argument doesn't take a separate value. Write -onlyCapture=false instead of -onlyCapture false."}},
		{"argument without its leading dash", []string{"-p", "123", "-appLogLineCount", "-1", "cmdTimeout", "1s"},
			[]string{`"cmdTimeout" is missing its leading "-", so the yc-360 script would ignore it. Write -cmdTimeout 1s.`, "About -cmdTimeout:"}},
		{"true/false argument without its leading dash", []string{"-p", "123", "onlyCapture"},
			[]string{`"onlyCapture" is missing its leading "-"`, "Write -onlyCapture."}},
		{"dashless name=value in the wrong case", []string{"-onlyCapture", "cmdtimeout=1s"},
			[]string{`"cmdtimeout=1s" is missing its leading "-"`, "Write -cmdTimeout=1s."}},
		{"dashless argument before other arguments", []string{"cmdTimeout", "1s", "-p", "123"},
			[]string{`"cmdTimeout" is missing its leading "-"`, "Write -cmdTimeout 1s."}},
		{"dashless argument with no value after it", []string{"-p", "123", "cmdTimeout"},
			[]string{"Write -cmdTimeout <value>."}},
		{"wrong case", []string{"-appruntime", "java"},
			[]string{"-appruntime is not a yc-360 script argument. Did you mean -appRuntime? Argument names are case-sensitive."}},
		{"unknown", []string{"-bogus"},
			[]string{"-bogus is not a yc-360 script argument. Check the spelling."}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			GlobalConfig = defaultConfig()
			err := ParseFlags(append([]string{"yc"}, c.args...))
			var argErr *ArgError
			require.True(t, errors.As(err, &argErr), "want *ArgError, got %v", err)
			for _, w := range c.want {
				assert.Contains(t, argErr.Msg, w)
			}
			assert.NotContains(t, argErr.Msg, "flag needs an argument")
		})
	}
}

func TestParseFlagsValidArgumentsStillParse(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })
	GlobalConfig = defaultConfig()

	// Negative numbers and values that merely start with "-" are values, not flags.
	require.NoError(t, ParseFlags([]string{"yc", "-appLogLineCount", "-1", "-a", "-my-app", "-p", "123", "-onlyCapture"}))
	assert.Equal(t, -1, GlobalConfig.AppLogLineCount)
	assert.Equal(t, "-my-app", GlobalConfig.AppName)
	assert.Equal(t, "123", GlobalConfig.Pid)
	assert.True(t, GlobalConfig.OnlyCapture)
}

func TestParseFlagsStrayWordsOnlyWarn(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original; ArgWarnings = nil })

	cases := []struct {
		args []string
		want string
	}{
		// Nothing after the stray word is lost, so the run continues as before.
		{[]string{"-p", "123", "-a", "First", "App"},
			`Ignored "App": it is not an argument or the value of one. If it is part of the -a value, wrap the whole value in double quotes, for example -a "First App".`},
		{[]string{"-p", "123", "-onlyCapture", "true"},
			`Ignored "true": it is not an argument or the value of one. Write -onlyCapture=true instead of -onlyCapture true.`},
		// A number can't contain spaces, so there is no quoting hint after one.
		{[]string{"-p", "123", "-appLogLineCount", "-1", "extra"},
			`Ignored "extra": it is not an argument or the value of one. Remove it, or wrap a value that contains spaces in double quotes.`},
		// After a text value, a word that is also an argument name is more
		// likely part of an unquoted value than a missing dash.
		{[]string{"-p", "123", "-a", "Web", "port"},
			`Ignored "port": it is not an argument or the value of one. If it is part of the -a value, wrap the whole value in double quotes, for example -a "Web port".`},
	}
	for _, c := range cases {
		GlobalConfig = defaultConfig()
		require.NoError(t, ParseFlags(append([]string{"yc"}, c.args...)), c.args)
		assert.Equal(t, []string{c.want}, ArgWarnings, c.args)
		assert.Equal(t, "123", GlobalConfig.Pid)
	}

	// A clean command line leaves no warning behind from an earlier parse.
	GlobalConfig = defaultConfig()
	require.NoError(t, ParseFlags([]string{"yc", "-p", "123"}))
	assert.Empty(t, ArgWarnings)

	// "--" ends the arguments on purpose.
	GlobalConfig = defaultConfig()
	require.NoError(t, ParseFlags([]string{"yc", "-p", "123", "--", "extra"}))
	assert.Empty(t, ArgWarnings)
}

func TestParseFlagsHelp(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })
	GlobalConfig = defaultConfig()

	for _, arg := range []string{"-h", "-help", "--help"} {
		assert.ErrorIs(t, ParseFlags([]string{"yc", arg}), ErrHelp, arg)
	}
}
