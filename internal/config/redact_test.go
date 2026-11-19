package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Placeholder key shape required by .gitleaks.toml for fixtures.
const testKey = "user@00000000-0000-0000-0000-000000000000"

func TestRedactArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"space separated", []string{"yc", "-p", "123", "-k", testKey, "-s", "http://x"},
			[]string{"yc", "-p", "123", "-k", redactedValue, "-s", "http://x"}},
		{"equals form", []string{"yc", "-k=" + testKey}, []string{"yc", "-k=" + redactedValue}},
		{"double dash", []string{"yc", "--k", testKey}, []string{"yc", "--k", redactedValue}},
		{"double dash equals", []string{"yc", "--k=" + testKey}, []string{"yc", "--k=" + redactedValue}},
		{"other sensitive flag", []string{"yc", "-boomiPassword", "secret"}, []string{"yc", "-boomiPassword", redactedValue}},
		{"no secrets unchanged", []string{"yc", "-p", "123", "-j", `D:\JDK\jdk-17`}, []string{"yc", "-p", "123", "-j", `D:\JDK\jdk-17`}},
		{"flag that only starts with k unchanged", []string{"yc", "-kernelParams", "x"}, []string{"yc", "-kernelParams", "x"}},
		{"-k as the last arg", []string{"yc", "-k"}, []string{"yc", "-k"}},
		// Windows merged the rest of the command line into -j (quoted path ending in \).
		{"merged into another value", []string{"yc", "-j", `D:\JDK\bin" -p 123 -k ` + testKey + ` -s http://x`},
			[]string{"yc", "-j", `D:\JDK\bin" -p 123 -k ` + redactedValue + ` -s http://x`}},
		{"merged with equals", []string{"yc", "-j", `D:\JDK\bin" -k=` + testKey},
			[]string{"yc", "-j", `D:\JDK\bin" -k=` + redactedValue}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := append([]string(nil), c.in...)
			assert.Equal(t, c.want, RedactArgs(c.in))
			assert.Equal(t, in, c.in, "input must not be modified")
		})
	}
}

func TestEffectiveConfigMessageShowsLoadedTokens(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })

	path := filepath.Join(t.TempDir(), "config.yaml")
	yamlText := "version: '1'\noptions:\n  k: " + testKey + "\n  s: http://localhost:8085\n  processTokens:\n    - launcher.jar$myApp\n"
	require.NoError(t, os.WriteFile(path, []byte(yamlText), 0o600))

	GlobalConfig = defaultConfig()
	require.NoError(t, ParseFlags([]string{"yc", "-c", path, "-m3"}))

	msg := EffectiveConfigMessage()
	assert.True(t, strings.HasPrefix(msg, "Effective configuration ((default)=matches built-in default, (set)=differs from default):\n"))
	assert.Contains(t, msg, "processTokens: [launcher.jar$myApp] (set)")
	assert.Contains(t, msg, "m3: true (set)")
	assert.Contains(t, msg, "k: "+redactedValue+" (set)")
	assert.NotContains(t, msg, "00000000-0000", "API key must not appear in the log")
}

func TestUsageDoesNotPrintSensitiveDefaults(t *testing.T) {
	original := GlobalConfig
	t.Cleanup(func() { GlobalConfig = original })

	// Flags take their current values as defaults, e.g. a key loaded from YAML.
	GlobalConfig.ApiKey = testKey
	GlobalConfig.Server = "http://localhost:8085"

	fs, _ := registerFlags("yc")
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.Usage()

	out := buf.String()
	assert.NotContains(t, out, "00000000-0000", "API key must not appear in help text")
	assert.Contains(t, out, `(default "`+redactedValue+`")`)
	// Non-sensitive defaults are still shown.
	assert.Contains(t, out, "http://localhost:8085")
	assert.True(t, strings.HasPrefix(out, "Usage of yc:"))

	// Parsing still uses the real value: an unset -k keeps the current key.
	fs2, result := registerFlags("yc")
	assert.NoError(t, fs2.Parse([]string{"-p", "123"}))
	for _, v := range result {
		if p, ok := v.(*string); ok && *p == testKey {
			return
		}
	}
	t.Fatal("registered -k default must still be the real key for parsing")
}
