package mysql

import "time"

// Collectors is every my_* artifact for one run. Within a connection, order is
// sampling order: the log tail first (its offset predates our own statements),
// my_explain after the digest read it walks, my_metadata before the whole-table reads.
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
