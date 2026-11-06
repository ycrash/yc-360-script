package runtime

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeForExecutableName(t *testing.T) {
	cases := map[string]RuntimeType{
		"java":        RuntimeJava,
		"JAVA.EXE":    RuntimeJava,
		"javaw.exe":   RuntimeJava,
		"node":        RuntimeNodeJS,
		"node.exe":    RuntimeNodeJS,
		"dotnet":      RuntimeDotNet,
		" dotnet.exe": RuntimeDotNet,
		// Custom launchers and embedded runtimes are never guessed.
		"tomcat9.exe": RuntimeUnknown,
		"electron":    RuntimeUnknown,
		"":            RuntimeUnknown,
	}
	for name, want := range cases {
		assert.Equal(t, want, RuntimeForExecutableName(name), name)
	}
}

func TestDetectProcessRuntimeUnknownForTestBinary(t *testing.T) {
	// The test binary is not a Java, Node.js or .NET launcher.
	rt, exe := DetectProcessRuntime(os.Getpid())
	assert.Equal(t, RuntimeUnknown, rt)
	assert.NotEmpty(t, exe)

	rt, _ = DetectProcessRuntime(-1)
	assert.Equal(t, RuntimeUnknown, rt)
}

func TestCleanExecutableName(t *testing.T) {
	cases := map[string]string{
		"/usr/lib/jvm/jdk-17/bin/java":           "java",
		"/usr/lib/jvm/jdk-17/bin/java (deleted)": "java",
		"/opt/node/bin/node (deleted)":           "node",
		"/opt/my app/bin/java":                   "java",
		"java":                                   "java",
	}
	for in, want := range cases {
		assert.Equal(t, want, cleanExecutableName(in), in)
	}
}
