package mysql

import "regexp"

// logPrefix opens a MySQL 8 error-log entry:
//
//	2026-10-27T13:06:05.623849Z 0 [System] [MY-010931] [Server] message
//
// InnoDB monitor and deadlock dumps bypass the sink, so an unprefixed line
// continues the previous entry.
var logPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2}) `)

// logEntry captures an entry's priority and error code.
var logEntry = regexp.MustCompile(`^\S+ \d+ \[(\w+)\] \[(MY-\d+)\]`)

// hasLogPrefix reports a line that opens a logged entry.
func hasLogPrefix(line string) bool { return logPrefix.MatchString(line) }

// entryCode returns an entry's code (MY-012468) and priority (Note); empty for a raw line.
func entryCode(line string) (code, priority string) {
	m := logEntry.FindStringSubmatch(line)
	if m == nil {
		return "", ""
	}

	return m[2], m[1]
}
