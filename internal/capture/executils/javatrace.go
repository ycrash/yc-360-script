package executils

import (
	"regexp"
	"strings"
)

// javaStackFrame matches the frame lines of a Java stack trace, such as
// "	at jdk.attach/sun.tools.attach.HotSpotAttachProvider.testAttachable(HotSpotAttachProvider.java:154)"
// and "	... 5 more".
var javaStackFrame = regexp.MustCompile(`^\s+(at \S+\(.*\)|\.\.\. \d+ more)\s*$`)

// TrimJavaStackTrace drops the frame lines of Java stack traces from command
// output used in log and error messages, keeping the exception line itself
// (e.g. "AttachNotSupportedException: The VM does not support the attach
// mechanism"). Only for messages: captured data is never trimmed.
func TrimJavaStackTrace(output string) string {
	if !strings.Contains(output, "\tat ") && !strings.Contains(output, " at ") {
		return output
	}
	lines := strings.Split(output, "\n")
	kept := lines[:0]
	for _, line := range lines {
		if javaStackFrame.MatchString(strings.TrimRight(line, "\r")) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
