package java

import "strings"

// GetFileFromJEP158 takes the file name from the JEP158 options
// For example from: /tmp/jvm.log:time,uptime,level,tags:filecount=10,filesize=1m
// It will return /tmp/jvm.log
// See also: https://openjdk.org/jeps/158
func GetFileFromJEP158(s string) string {
	strBuilder := strings.Builder{}

	// Handle Windows's drive character `:\`
	// Without this handling, the `C:\` string confused the logic below this.
	if strings.Contains(s, `:\`) {
		splitted := strings.SplitAfterN(s, `:\`, 2)

		// Put the `C:\`` to strBuilder for later
		strBuilder.WriteString(splitted[0])

		// Continue the logic as usual without the `C:\`
		s = splitted[1]
	} else if strings.Contains(s, `:/`) {
		// Handle strange case:
		// -Xlog:gc*:file=\"F:/tmp/psslogs/gc.log\":tags,time,uptime,level:filecount=10,filesize=10M
		// or
		// -Xlog:gc*:file="F:/tmp/psslogs/gc.log":tags,time,uptime,level:filecount=10,filesize=10M
		// where the slash is F:/ instead of F:\

		splitted := strings.SplitAfterN(s, `:/`, 2)

		// Put the `C:/`` to strBuilder for later
		strBuilder.WriteString(splitted[0])

		// Continue the logic as usual without the `C:/`
		s = splitted[1]
	}

	splitted := strings.SplitN(s, ":", 2)
	if len(splitted) > 0 {
		strBuilder.WriteString(splitted[0])
	} else {
		strBuilder.WriteString(s)
	}

	logFile := strBuilder.String()

	// Remove extra \" such in
	// -Xlog:gc*:file=\"F:/tmp/psslogs/gc.log\":tags,time,uptime,level:filecount=10,filesize=10M
	logFile = strings.TrimPrefix(strings.TrimSuffix(logFile, `\"`), `\"`)

	// Remove extra " such in
	// -Xlog:gc*:file="F:/tmp/psslogs/gc.log":tags,time,uptime,level:filecount=10,filesize=10M
	logFile = strings.Trim(logFile, `" `)

	return logFile
}
