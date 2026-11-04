package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// javaHomeSeparators are the characters treated as path separators in a Java
// home path. Windows accepts both; on POSIX a backslash is a normal file-name
// character.
func javaHomeSeparators() string {
	if runtime.GOOS == "windows" {
		return `/\`
	}
	return "/"
}

// trimJavaHomeBin removes a trailing "bin" folder from a Java home path.
//
// Every Java tool path is built as <javaHome>/bin/<tool>, so a -j or
// JAVA_HOME value that already points at the bin folder (e.g. D:\jdk\bin)
// would make yc look for D:\jdk\bin\bin\jcmd. It returns the corrected path
// and true when it removed "bin", or the input unchanged and false otherwise.
// On Windows the match ignores case (BIN, Bin).
func trimJavaHomeBin(javaHome string) (string, bool) {
	seps := javaHomeSeparators()
	trimmed := strings.TrimRight(javaHome, seps)
	i := strings.LastIndexAny(trimmed, seps)
	if i < 0 {
		// A bare "bin" (relative, no parent) is left alone.
		return javaHome, false
	}

	last := trimmed[i+1:]
	isBin := last == "bin" || (runtime.GOOS == "windows" && strings.EqualFold(last, "bin"))
	if !isBin {
		return javaHome, false
	}

	parent := strings.TrimRight(trimmed[:i], seps)
	if parent == "" || parent == filepath.VolumeName(parent) {
		// "/bin" or "D:\bin": the Java home is the root itself. Keep the
		// separator, because on Windows a bare "D:" means the current folder
		// on drive D, not its root.
		parent = trimmed[:i+1]
	}
	return parent, true
}

// normalizeJavaHome cleans up a -j / JAVA_HOME value before tools are looked
// up under it. On Windows it first drops stray quotes and spaces at either end,
// since a Windows path can't contain a quote:
//   - a quoted argument ending in a backslash, such as -j "C:\Program Files\jdk\bin\"
//     when it is the last argument, reaches the process as `C:\Program Files\jdk\bin" `
//     because Windows treats \" as an escaped quote;
//   - `set JAVA_HOME="C:\jdk"` in cmd.exe keeps the quotes in the value.
//
// It then removes a trailing bin folder (see trimJavaHomeBin). It returns the
// cleaned path and whether anything changed.
func normalizeJavaHome(javaHome string) (string, bool) {
	cleaned := javaHome
	if runtime.GOOS == "windows" {
		cleaned = strings.Trim(cleaned, `" `)
	}
	cleaned, _ = trimJavaHomeBin(cleaned)
	return cleaned, cleaned != javaHome
}

// javaHomeSwallowedArgs reports whether a -j value shows that Windows merged
// the following command-line arguments into it, and returns the path part
// (the text before the quote).
//
// On Windows, \" inside a quoted argument is a literal quote, so
// -j "C:\jdk\bin\" -p 123 -s URL arrives as the single value
// `C:\jdk\bin" -p 123 -s URL`. The later arguments are lost and can't be
// recovered, so the caller should stop with a clear message rather than fail
// later on a seemingly missing -p, -s or -k. A quote that is still inside the
// value after stripping the ends is the sign; a Windows path can't contain one.
//
// Only the path part is returned for logging: the merged text can contain
// later arguments such as the -k API key, which must not be printed.
func javaHomeSwallowedArgs(javaHome string) (pathPart string, swallowed bool) {
	if runtime.GOOS != "windows" {
		return javaHome, false
	}
	before, _, found := strings.Cut(strings.Trim(javaHome, `" `), `"`)
	if !found {
		return javaHome, false
	}
	return before, true
}

// absJavaHome makes a relative Java home path absolute.
//
// The on-demand and M3 captures chdir into a yc-<timestamp> capture folder
// before running any JDK tool, so a relative path such as -j jdk17 would be
// looked up inside that folder. Resolving it at startup keeps it pointing
// where the user meant. Rooted paths ("/opt/jdk", or "\jdk" on Windows) and
// paths with a volume ("D:jdk") are left alone: a chdir within the same drive
// doesn't change what they mean. It returns the new path and whether it changed.
func absJavaHome(javaHome string) (string, bool) {
	if javaHome == "" || filepath.IsAbs(javaHome) || filepath.VolumeName(javaHome) != "" ||
		strings.ContainsAny(javaHome[:1], javaHomeSeparators()) {
		return javaHome, false
	}
	abs, err := filepath.Abs(javaHome)
	if err != nil {
		return javaHome, false
	}
	return abs, true
}

// javaExecutableExists reports whether <javaHome>/bin/java (java.exe on
// Windows) exists, as a quick sanity check of the configured Java home.
func javaExecutableExists(javaHome string) bool {
	name := "java"
	if runtime.GOOS == "windows" {
		name = "java.exe"
	}
	info, err := os.Stat(filepath.Join(javaHome, "bin", name))
	return err == nil && !info.IsDir()
}
