package mysql

import "time"

// Collectors is every my_* artifact for one run, in registration order. Each names
// the connection for its speed, and the three connections' timelines run at the same
// time. Within one, registration order is sampling order on a shared tick: the log
// tail goes first on the normal connection, so its starting offset is taken before
// that connection's own statements; then the cheap counter reads; then the
// digest read, and my_explain last, since it walks that read every tick. On the
// expensive connection my_metadata is read once, at the start, before the
// whole-table reads.
func Collectors(metadata *MetadataCollector, explainMode string, fast, normal, expensive time.Duration) []Collector {
	slowQueries := &SlowQueries{Interval: normal}

	return []Collector{
		NewSessions(fast),
		NewLockWaits(fast),

		&Deadlocks{Interval: LogTailInterval},
		NewCapacity(normal),
		NewErrors(normal),
		NewTimeouts(normal),
		NewMemory(normal),
		&BufferPool{Interval: normal},
		&Replication{Interval: normal},
		NewPreparedStatements(normal),
		slowQueries,
		NewExplain(explainMode, slowQueries, normal, metadata.target.Database),

		metadata,
		NewNonDefaultSettings(expensive),
		NewTableStats(expensive),
		NewStorage(expensive),
	}
}
