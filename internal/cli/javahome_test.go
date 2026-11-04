package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"yc-agent/internal/config"
	"yc-agent/internal/logger"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrimJavaHomeBin(t *testing.T) {
	type tc struct {
		name    string
		in      string
		want    string
		trimmed bool
	}
	cases := []tc{
		{"posix bin", "/usr/lib/jvm/java-17/bin", "/usr/lib/jvm/java-17", true},
		{"posix bin trailing slash", "/usr/lib/jvm/java-17/bin/", "/usr/lib/jvm/java-17", true},
		{"posix home unchanged", "/usr/lib/jvm/java-17", "/usr/lib/jvm/java-17", false},
		{"posix home trailing slash unchanged", "/usr/lib/jvm/java-17/", "/usr/lib/jvm/java-17/", false},
		{"folder merely ending in bin unchanged", "/opt/jdkbin", "/opt/jdkbin", false},
		{"bin in the middle unchanged", "/opt/bin/jdk", "/opt/bin/jdk", false},
		{"root bin", "/bin", "/", true},
		{"bare bin unchanged", "bin", "bin", false},
		{"empty unchanged", "", "", false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			tc{"windows bin", `D:\JDK\jdk-17.0.6\bin`, `D:\JDK\jdk-17.0.6`, true},
			tc{"windows bin trailing backslash", `D:\JDK\jdk-17.0.6\bin\`, `D:\JDK\jdk-17.0.6`, true},
			tc{"windows forward slashes", `D:/JDK/jdk-17.0.6/bin`, `D:/JDK/jdk-17.0.6`, true},
			tc{"windows upper-case BIN", `D:\JDK\jdk-17.0.6\BIN`, `D:\JDK\jdk-17.0.6`, true},
			tc{"windows home unchanged", `D:\JDK\jdk-17.0.6`, `D:\JDK\jdk-17.0.6`, false},
			// A bare "D:" would mean the current folder on drive D, so keep the root separator.
			tc{"windows drive root bin", `D:\bin`, `D:\`, true},
			tc{"windows drive root bin forward slash", `D:/bin/`, `D:/`, true},
			tc{"windows UNC share bin", `\\server\share\jdk\bin`, `\\server\share\jdk`, true},
		)
	} else {
		cases = append(cases,
			// Case matters on POSIX file systems, so BIN is a different folder.
			tc{"posix upper-case BIN unchanged", "/opt/jdk/BIN", "/opt/jdk/BIN", false},
		)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, trimmed := trimJavaHomeBin(c.in)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.trimmed, trimmed)
		})
	}
}

func TestNormalizeJavaHome(t *testing.T) {
	type tc struct {
		name    string
		in      string
		want    string
		changed bool
	}
	cases := []tc{
		{"bin removed", "/opt/jdk/bin", "/opt/jdk", true},
		{"home unchanged", "/opt/jdk", "/opt/jdk", false},
	}
	if runtime.GOOS == "windows" {
		cases = append(cases,
			// -j "D:\JDK\jdk-17.0.6\bin\" arrives with \" read as a literal quote.
			tc{"stray quote from quoted trailing backslash", `D:\JDK\jdk-17.0.6\bin" `, `D:\JDK\jdk-17.0.6`, true},
			tc{"stray quote without bin", `C:\Program Files\Java\jdk-17" `, `C:\Program Files\Java\jdk-17`, true},
			tc{"spaces inside path kept", `C:\Program Files\Java\jdk-17`, `C:\Program Files\Java\jdk-17`, false},
			tc{"only a stray quote becomes empty", `" `, ``, true},
			// cmd.exe: set JAVA_HOME="C:\jdk" keeps both quotes in the value.
			tc{"surrounding quotes from set JAVA_HOME", `"C:\Program Files\Java\jdk-17"`, `C:\Program Files\Java\jdk-17`, true},
		)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, changed := normalizeJavaHome(c.in)
			assert.Equal(t, c.want, got)
			assert.Equal(t, c.changed, changed)
		})
	}
}

// validate() must apply the normalization to both -j and the JAVA_HOME fallback.
func TestValidateNormalizesJavaHome(t *testing.T) {
	originalConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = originalConfig })
	logger.Init("", 0, 0, "info")

	home := t.TempDir()
	binDir := filepath.Join(home, "bin")

	t.Run("-j pointing at bin", func(t *testing.T) {
		t.Setenv("JAVA_HOME", "")
		config.GlobalConfig = config.Config{Options: config.Options{OnlyCapture: true, JavaHomePath: binDir}}
		require.NoError(t, validate())
		assert.Equal(t, home, config.GlobalConfig.JavaHomePath)
	})

	t.Run("JAVA_HOME pointing at bin", func(t *testing.T) {
		t.Setenv("JAVA_HOME", binDir)
		config.GlobalConfig = config.Config{Options: config.Options{OnlyCapture: true}}
		require.NoError(t, validate())
		assert.Equal(t, home, config.GlobalConfig.JavaHomePath)
	})

	t.Run("relative -j made absolute", func(t *testing.T) {
		t.Setenv("JAVA_HOME", "")
		t.Chdir(filepath.Dir(home))
		config.GlobalConfig = config.Config{Options: config.Options{OnlyCapture: true,
			JavaHomePath: filepath.Join(filepath.Base(home), "bin")}}
		require.NoError(t, validate())
		assert.Equal(t, home, config.GlobalConfig.JavaHomePath)
	})

	t.Run("normal home unchanged", func(t *testing.T) {
		t.Setenv("JAVA_HOME", "")
		config.GlobalConfig = config.Config{Options: config.Options{OnlyCapture: true, JavaHomePath: home}}
		require.NoError(t, validate())
		assert.Equal(t, home, config.GlobalConfig.JavaHomePath)
	})
}

func TestJavaHomeSwallowedArgs(t *testing.T) {
	merged := `D:\JDK\jdk-17.0.6\bin" -p 123 -k user@00000000-0000-0000-0000-000000000000`
	swallowed := func(v string) bool { _, s := javaHomeSwallowedArgs(v); return s }
	if runtime.GOOS != "windows" {
		// Shells on POSIX don't have this quoting rule; never flag there.
		assert.False(t, swallowed(merged))
		return
	}
	pathPart, ok := javaHomeSwallowedArgs(merged)
	assert.True(t, ok, "arguments merged after a literal quote")
	// Only the path is returned for logging, never the merged arguments (API key).
	assert.Equal(t, `D:\JDK\jdk-17.0.6\bin`, pathPart)
	assert.NotContains(t, pathPart, "00000000")

	assert.False(t, swallowed(`D:\JDK\jdk-17.0.6\bin" `), "-j last: only a trailing quote, fixed by normalizeJavaHome")
	assert.False(t, swallowed(`"C:\Program Files\Java\jdk-17"`), "surrounding quotes only")
	assert.False(t, swallowed(`C:\Program Files\Java\jdk-17`))
	assert.False(t, swallowed(``))
}

func TestAbsJavaHome(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	got, changed := absJavaHome("jdk17")
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(cwd, "jdk17"), got)

	got, changed = absJavaHome(filepath.Join(".", "tools", "jdk"))
	assert.True(t, changed)
	assert.Equal(t, filepath.Join(cwd, "tools", "jdk"), got)

	for _, unchanged := range []string{"", "/opt/jdk", cwd} {
		got, changed = absJavaHome(unchanged)
		assert.False(t, changed, unchanged)
		assert.Equal(t, unchanged, got)
	}
	if runtime.GOOS == "windows" {
		for _, unchanged := range []string{`\JDK\jdk-17`, `D:jdk`, `D:\JDK\jdk-17`, `\\server\share\jdk`} {
			got, changed = absJavaHome(unchanged)
			assert.False(t, changed, unchanged)
			assert.Equal(t, unchanged, got)
		}
	}
}

func TestValidateRejectsSwallowedArgs(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows-only quoting rule")
	}
	originalConfig := config.GlobalConfig
	t.Cleanup(func() { config.GlobalConfig = originalConfig })
	logger.Init("", 0, 0, "info")

	// OnlyCapture keeps every other check passing, so only the -j check can fail this.
	config.GlobalConfig = config.Config{Options: config.Options{
		OnlyCapture:  true,
		JavaHomePath: `D:\JDK\jdk-17.0.6\bin" -p 123 -onlyCapture`,
	}}
	assert.Equal(t, ErrInvalidArgumentCantContinue, validate())

	// Control: the same config with a clean -j passes.
	config.GlobalConfig = config.Config{Options: config.Options{
		OnlyCapture:  true,
		JavaHomePath: `D:\JDK\jdk-17.0.6\bin" `,
	}}
	assert.NoError(t, validate())
}

func TestJavaExecutableExists(t *testing.T) {
	home := t.TempDir()
	assert.False(t, javaExecutableExists(home), "empty dir has no bin/java")

	name := "java"
	if runtime.GOOS == "windows" {
		name = "java.exe"
	}
	assert.NoError(t, os.MkdirAll(filepath.Join(home, "bin"), 0o755))
	assert.NoError(t, os.WriteFile(filepath.Join(home, "bin", name), []byte{}, 0o755))
	assert.True(t, javaExecutableExists(home))

	// The common mistake: pointing at bin itself.
	assert.False(t, javaExecutableExists(filepath.Join(home, "bin")))
}
