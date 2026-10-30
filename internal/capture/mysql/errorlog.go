package mysql

import "regexp"

// logPrefix opens every entry the server writes through its log sink, in the
// traditional format of MySQL 8:
//
//	2026-10-27T13:06:05.623849Z 0 [System] [MY-010931] [Server] message
//
// InnoDB's monitor output and deadlock dumps are written past the sink, as raw
// lines with no prefix, so a line without one continues the entry before it.
var logPrefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2}) `)

// logEntry is the prefix's fields: the error code and priority an entry was logged
// under.
var logEntry = regexp.MustCompile(`^\S+ \d+ \[(\w+)\] \[(MY-\d+)\]`)

// hasLogPrefix reports a line that opens a logged entry.
func hasLogPrefix(line string) bool { return logPrefix.MatchString(line) }

// entryCode is a logged entry's error code (MY-012468) and priority (Note), empty
// for a raw line.
func entryCode(line string) (code, priority string) {
	m := logEntry.FindStringSubmatch(line)
	if m == nil {
		return "", ""
	}

	return m[2], m[1]
}
