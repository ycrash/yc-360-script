package executils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTrimJavaStackTrace(t *testing.T) {
	// jcmd output on Windows, with CRLF line endings.
	jcmd := "308856:\r\n" +
		"com.sun.tools.attach.AttachNotSupportedException: The VM does not support the attach mechanism\r\n" +
		"\tat jdk.attach/sun.tools.attach.HotSpotAttachProvider.testAttachable(HotSpotAttachProvider.java:154)\r\n" +
		"\tat jdk.jcmd/sun.tools.jcmd.JCmd.main(JCmd.java:97)\r\n" +
		"Caused by: java.io.IOException: boom\r\n" +
		"        at java.base/java.io.Foo.bar(Native Method)\r\n" +
		"\t... 5 more\r\n"
	assert.Equal(t, "308856:\r\n"+
		"com.sun.tools.attach.AttachNotSupportedException: The VM does not support the attach mechanism\r\n"+
		"Caused by: java.io.IOException: boom\r\n",
		TrimJavaStackTrace(jcmd))

	// Output that isn't a stack trace is left alone, including "at" in prose.
	for _, s := range []string{
		"",
		"Connected to remote process\nAttach is not supported by the target process (error code = 100)\n",
		"Recording started at 10:00 (see file)",
		"  look at this (not a frame)",
	} {
		assert.Equal(t, s, TrimJavaStackTrace(s))
	}
}
