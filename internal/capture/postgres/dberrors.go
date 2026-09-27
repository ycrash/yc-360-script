package postgres

// errorMatch takes every entry logged at ERROR, FATAL or PANIC, with the rest of its
// event, less what pg_deadlocks.txt and pg_timeouts.txt already copy: no event is
// written twice. The levels are the English keywords every tail reads.
var errorMatch = eventMatch{
	severity: []string{"ERROR", "FATAL", "PANIC"},
	exclude:  []eventMatch{deadlockMatch, timeoutMatch},
}
