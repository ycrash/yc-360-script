package mysql

import "time"

// Row bounds for the fast files, whose sources have no natural limit: a server can
// hold thousands of sessions, and a lock storm many more lock rows.
const (
	DefaultMaxSessions = 1000
	DefaultMaxLocks    = 5000

	// DefaultMaxPreparedStatements bounds my_prepared_statements.txt;
	// max_prepared_stmt_count defaults to 16382.
	DefaultMaxPreparedStatements = 5000
)

// NewSessions is my_sessions.txt: every foreground thread with its client's
// program_name - the agent's own included, as an ordinary column (spec §1.3) - and
// every InnoDB transaction. performance_schema.threads, not
// information_schema.processlist, which contends on a global lock under load.
// Without PROCESS the role sees only its own threads, and innodb_trx is refused.
func NewSessions(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_sessions", ConnectionFast, interval),
		blocks: func(SampleContext) []block {
			return []block{
				{
					source: "threads",
					query: limitSQL(`SELECT t.THREAD_ID, t.PROCESSLIST_ID, t.PROCESSLIST_USER, t.PROCESSLIST_HOST,
       t.PROCESSLIST_DB, t.PROCESSLIST_COMMAND, t.PROCESSLIST_TIME, t.PROCESSLIST_STATE,
       t.PROCESSLIST_INFO, a.ATTR_VALUE AS program_name
FROM performance_schema.threads t
LEFT JOIN performance_schema.session_connect_attrs a
  ON t.PROCESSLIST_ID = a.PROCESSLIST_ID AND a.ATTR_NAME = 'program_name'
WHERE t.TYPE = 'FOREGROUND'
ORDER BY t.THREAD_ID`, DefaultMaxSessions),
					columns: []string{"THREAD_ID", "PROCESSLIST_ID", "PROCESSLIST_USER", "PROCESSLIST_HOST",
						"PROCESSLIST_DB", "PROCESSLIST_COMMAND", "PROCESSLIST_TIME", "PROCESSLIST_STATE",
						"PROCESSLIST_INFO", "program_name"},
					statementColumns: []string{"PROCESSLIST_INFO"},
					maxRows:          DefaultMaxSessions,
				},
				{
					source: "innodb_trx",
					query: limitSQL(`SELECT trx_id, trx_state, trx_started, trx_mysql_thread_id
FROM information_schema.innodb_trx
ORDER BY trx_started`, DefaultMaxSessions),
					columns: []string{"trx_id", "trx_state", "trx_started", "trx_mysql_thread_id"},
					maxRows: DefaultMaxSessions,
				},
			}
		},
	}
}

// NewLockWaits is my_lock_waits.txt: row lock waits, and pending metadata locks -
// an ALTER TABLE queued behind an open transaction holds no row lock at all.
// Sessions are named by thread id, which my_sessions.txt resolves.
func NewLockWaits(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_lock_waits", ConnectionFast, interval),
		blocks: func(SampleContext) []block {
			return []block{
				{
					source: "data_lock_waits",
					query: limitSQL(`SELECT ENGINE, REQUESTING_ENGINE_LOCK_ID, REQUESTING_ENGINE_TRANSACTION_ID,
       REQUESTING_THREAD_ID, BLOCKING_ENGINE_LOCK_ID, BLOCKING_ENGINE_TRANSACTION_ID, BLOCKING_THREAD_ID
FROM performance_schema.data_lock_waits`, DefaultMaxLocks),
					columns: []string{"ENGINE", "REQUESTING_ENGINE_LOCK_ID", "REQUESTING_ENGINE_TRANSACTION_ID",
						"REQUESTING_THREAD_ID", "BLOCKING_ENGINE_LOCK_ID", "BLOCKING_ENGINE_TRANSACTION_ID",
						"BLOCKING_THREAD_ID"},
					maxRows: DefaultMaxLocks,
				},
				{
					source: "metadata_locks",
					query: limitSQL(`SELECT OBJECT_TYPE, OBJECT_SCHEMA, OBJECT_NAME, LOCK_TYPE, LOCK_STATUS, OWNER_THREAD_ID
FROM performance_schema.metadata_locks
WHERE LOCK_STATUS = 'PENDING'`, DefaultMaxLocks),
					columns: []string{"OBJECT_TYPE", "OBJECT_SCHEMA", "OBJECT_NAME", "LOCK_TYPE", "LOCK_STATUS",
						"OWNER_THREAD_ID"},
					maxRows: DefaultMaxLocks,
				},
			}
		},
	}
}

// NewPreparedStatements is my_prepared_statements.txt: every statement prepared
// right now, by the thread holding it. The agent prepares none of its own: its
// statements carry no arguments, and the driver interpolates any that would.
func NewPreparedStatements(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_prepared_statements", ConnectionNormal, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "prepared_statements_instances",
				query: limitSQL(`SELECT OWNER_THREAD_ID, STATEMENT_ID, STATEMENT_NAME, SQL_TEXT, COUNT_EXECUTE,
       SUM_TIMER_EXECUTE
FROM performance_schema.prepared_statements_instances
ORDER BY OWNER_THREAD_ID, STATEMENT_ID`, DefaultMaxPreparedStatements),
				columns: []string{"OWNER_THREAD_ID", "STATEMENT_ID", "STATEMENT_NAME", "SQL_TEXT", "COUNT_EXECUTE",
					"SUM_TIMER_EXECUTE"},
				statementColumns: []string{"SQL_TEXT"},
				maxRows:          DefaultMaxPreparedStatements,
			}}
		},
	}
}

// NewTableStats is my_table_stats.txt: the 100 most-read index uses. An empty
// INDEX_NAME is a read that used no index - a full scan. rows_total= is how many
// there were, so a cut at 100 says so.
func NewTableStats(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_table_stats", ConnectionExpensive, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "table_io_waits_summary_by_index_usage",
				query: `SELECT OBJECT_SCHEMA, OBJECT_NAME, INDEX_NAME, COUNT_READ, COUNT_FETCH,
       ` + rowsTotalSelect + `
FROM performance_schema.table_io_waits_summary_by_index_usage
ORDER BY COUNT_READ DESC
LIMIT 100`,
				columns: []string{"OBJECT_SCHEMA", "OBJECT_NAME", "INDEX_NAME", "COUNT_READ", "COUNT_FETCH"},
				counted: true,
			}}
		},
	}
}

// NewStorage is my_storage.txt: the 100 largest tables outside the system schemas.
// information_schema.TABLES lists only tables the role holds some privilege on, so
// a role with none on the application's schemas reads an empty list. The sizes are
// the server's cached statistics, as old as information_schema_stats_expiry allows
// (my_metadata.txt records it): asking for fresh ones would open every table, on a
// database that may already be struggling. rows_total= is how many tables the role
// can see, so a cut at 100 says so.
func NewStorage(interval time.Duration) Collector {
	return &queryCollector{
		artifact: periodicArtifact("my_storage", ConnectionExpensive, interval),
		blocks: func(SampleContext) []block {
			return []block{{
				source: "tables",
				query: `SELECT TABLE_SCHEMA, TABLE_NAME, ENGINE, DATA_LENGTH, INDEX_LENGTH, DATA_FREE,
       ` + rowsTotalSelect + `
FROM information_schema.TABLES
WHERE TABLE_SCHEMA NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY DATA_LENGTH DESC
LIMIT 100`,
				columns: []string{"TABLE_SCHEMA", "TABLE_NAME", "ENGINE", "DATA_LENGTH", "INDEX_LENGTH", "DATA_FREE"},
				counted: true,
			}}
		},
	}
}
